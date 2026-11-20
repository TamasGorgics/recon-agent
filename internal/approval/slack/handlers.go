package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/approval"
	"github.com/TamasGorgics/recon-agent/internal/approval/state"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/outbox"
)

// Outbox topic keys. The cancel constant intentionally mirrors
// cases/lifecycle.TopicApprovalCancel (asserted by a test) so this adapter
// does not import the case lifecycle package for one string.
const (
	topicApprovalCancel = "approval.cancel"
)

// ErrInvalidPayload is returned for an outbox payload this adapter cannot
// parse or validate. The outbox worker records the failed attempt and applies
// its normal retry/dead-letter policy; handlers never panic on bad payloads.
var ErrInvalidPayload = errors.New("approval/slack: invalid outbox payload")

// ErrInvalidHandler reports an incomplete handler dependency set.
var ErrInvalidHandler = errors.New("approval/slack: invalid outbox handler options")

// HandlersOptions is the outbox dependency set. Queryer, Client, ChannelID,
// and CaseFileBaseURL are required. Source and Approvers default to a DBSource
// built from Queryer; Store defaults to the stateless repository. Clock and
// Logger default to clock.Real and slog.Default.
type HandlersOptions struct {
	// Queryer runs the short store statements. It is usually the recon_app
	// pool; no network call may run while a transaction on it is open.
	Queryer Queryer
	// Store reads and marks approval_messages rows. Nil means &Store{}.
	Store *Store
	// Source hydrates proposal/case content outside any transaction. Nil
	// means a DBSource over Queryer.
	Source Source
	// Approvers resolves the display name on a decided message. Nil falls
	// back to Source when it implements ApproverReader.
	Approvers ApproverReader
	// Client posts and updates Slack messages.
	Client *Client
	// ChannelID is the configured Slack channel for approval requests.
	ChannelID string
	// CaseFileBaseURL is the server-owned case-file link prefix.
	CaseFileBaseURL string
	// Clock supplies the deterministic "now" used for case age.
	Clock clock.Clock
	// Logger receives delivery diagnostics without payloads or secrets.
	Logger *slog.Logger
}

// Handlers delivers the Slack side of the approval outbox topics. It is
// stateless apart from its dependencies and safe for concurrent use: each
// handler performs short single-statement reads/writes and never opens a
// transaction across the Slack network call (FIN-22).
type Handlers struct {
	queryer         Queryer
	store           *Store
	source          Source
	approvers       ApproverReader
	client          *Client
	channelID       string
	caseFileBaseURL string
	clock           clock.Clock
	logger          *slog.Logger
}

// NewHandlers validates the dependency set and applies defaults.
func NewHandlers(opts HandlersOptions) (*Handlers, error) {
	if err := validateQueryer(opts.Queryer); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidHandler, err)
	}
	if opts.Client == nil {
		return nil, fmt.Errorf("%w: slack client is required", ErrInvalidHandler)
	}
	if err := validateChannel(opts.ChannelID); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidHandler, err)
	}
	caseFileBaseURL, err := validateCaseFileBaseURL(opts.CaseFileBaseURL)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidHandler, err)
	}

	source := opts.Source
	if source == nil {
		dbSource, err := NewSource(opts.Queryer)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidHandler, err)
		}
		source = dbSource
	}
	approvers := opts.Approvers
	if approvers == nil {
		if reader, ok := source.(ApproverReader); ok {
			approvers = reader
		}
	}
	store := opts.Store
	if store == nil {
		store = &Store{}
	}
	clk := opts.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Handlers{
		queryer:         opts.Queryer,
		store:           store,
		source:          source,
		approvers:       approvers,
		client:          opts.Client,
		channelID:       opts.ChannelID,
		caseFileBaseURL: caseFileBaseURL,
		clock:           clk,
		logger:          logger,
	}, nil
}

// Map returns the three outbox handlers keyed by topic. cmd/outbox registers
// the result directly in outbox.WorkerOptions.Handlers.
func (h *Handlers) Map() map[string]outbox.Handler {
	return map[string]outbox.Handler{
		approval.TopicApprovalRequest: outbox.HandlerFunc(h.HandleRequest),
		approval.TopicApprovalUpdate:  outbox.HandlerFunc(h.HandleUpdate),
		topicApprovalCancel:           outbox.HandlerFunc(h.HandleCancel),
	}
}

// NewHandlerMap builds the handlers and returns the topic map. It is the
// convenience entry point for cmd/outbox and integration tests.
func NewHandlerMap(opts HandlersOptions) (map[string]outbox.Handler, error) {
	h, err := NewHandlers(opts)
	if err != nil {
		return nil, err
	}
	return h.Map(), nil
}

// ApprovalRequestPayload is the approval.request outbox payload produced by
// internal/approval.Service.Request. It is exported only so tests and a future
// producer can share the contract without duplicating field names.
type ApprovalRequestPayload struct {
	ProposalID  string `json:"proposal_id"`
	CaseID      string `json:"case_id"`
	Tier        string `json:"tier"`
	Channel     string `json:"channel"`
	RequestedAt string `json:"requested_at"`
}

// ApprovalUpdatePayload is the approval.update outbox payload contract from
// spec section 6.1. Reason is optional. The producer lands in a later wave.
type ApprovalUpdatePayload struct {
	ProposalID    string `json:"proposal_id"`
	CaseID        string `json:"case_id"`
	Decision      string `json:"decision"`
	Status        string `json:"status"`
	Source        string `json:"source"`
	ApproverID    string `json:"approver_id"`
	DecidedAt     string `json:"decided_at"`
	InfoRequested bool   `json:"info_requested"`
	Reason        string `json:"reason"`
}

// ApprovalCancelPayload is the approval.cancel outbox payload produced by the
// case lifecycle on supersede/late-match/unmatch.
type ApprovalCancelPayload struct {
	ProposalID string `json:"proposal_id"`
	CaseID     string `json:"case_id"`
	Reason     string `json:"reason"`
}

// HandleRequest delivers one approval.request row.
//
// Ordering: check approval_messages first (any row means already handled),
// hydrate and re-check pending_approval, post with client_msg_id=proposal_id,
// then persist the external ref. No transaction spans the network call. After
// the post the proposal status is read again; a proposal that is no longer
// pending has its just-posted message withdrawn and the row recorded as
// cancelled, closing the spec section 6.2 cancel-before-insert window.
func (h *Handlers) HandleRequest(ctx context.Context, msg outbox.Message) error {
	payload, err := parseApprovalRequestPayload(msg.Payload)
	if err != nil {
		return err
	}
	if payload.Channel != approval.ChannelSlack {
		h.logger.Debug("approval/slack: skipping non-slack approval request",
			"proposal_id", payload.ProposalID, "channel", payload.Channel)
		return nil
	}

	existing, found, err := h.store.Get(ctx, h.queryer, payload.ProposalID, approval.ChannelSlack)
	if err != nil {
		return err
	}
	if found {
		h.logger.Debug("approval/slack: approval request already handled",
			"proposal_id", payload.ProposalID, "state", existing.State)
		return nil
	}

	data, err := h.source.ApprovalMessage(ctx, payload.ProposalID)
	if err != nil {
		return err
	}
	if data.ProposalStatus != string(state.StatusPendingApproval) {
		h.logger.Debug("approval/slack: proposal is no longer pending; skipping post",
			"proposal_id", payload.ProposalID, "status", data.ProposalStatus)
		return nil
	}

	opts := h.buildOptions()
	message, err := BuildMessage(data, opts)
	if err != nil {
		return fmt.Errorf("approval/slack: build request message for %s: %w",
			payload.ProposalID, err)
	}
	ref, err := h.client.PostMessageRef(ctx, h.channelID, message)
	if err != nil {
		return fmt.Errorf("approval/slack: post approval request for %s: %w",
			payload.ProposalID, err)
	}

	// The Slack call is complete; only now read the proposal again and write
	// transport state. A concurrent cancel that found no row before the post
	// cannot leave a live approval button behind (spec section 6.2).
	latest, err := h.source.ApprovalMessage(ctx, payload.ProposalID)
	if err != nil {
		return err
	}
	if latest.ProposalStatus != string(state.StatusPendingApproval) {
		withdrawn, err := buildWithdrawnMessage(latest, opts, "superseded before delivery")
		if err != nil {
			return fmt.Errorf("approval/slack: build withdrawn message for %s: %w",
				payload.ProposalID, err)
		}
		if err := h.updateRef(ctx, ref, withdrawn); err != nil {
			return fmt.Errorf("approval/slack: withdraw stale approval request for %s: %w",
				payload.ProposalID, err)
		}
		if err := h.recordCancelled(ctx, payload.ProposalID, ref); err != nil {
			return err
		}
		h.logger.Info("approval/slack: withdrew request for proposal that left pending",
			"proposal_id", payload.ProposalID, "status", latest.ProposalStatus)
		return nil
	}

	inserted, err := h.store.InsertPosted(ctx, h.queryer, payload.ProposalID,
		approval.ChannelSlack, ref)
	if err != nil {
		return err
	}
	if !inserted {
		// Another delivery won the post/insert race. client_msg_id is the
		// best-effort duplicate guard; the remaining window is accepted by
		// spec section 4.1/Q-97, so never withdraw the winner's message.
		h.logger.Debug("approval/slack: approval message already recorded",
			"proposal_id", payload.ProposalID)
		return nil
	}
	h.logger.Info("approval/slack: posted approval request",
		"proposal_id", payload.ProposalID, "channel", h.channelID, "external_ref", ref)
	return nil
}

// HandleUpdate delivers one approval.update row. An absent row means the
// request never reached Slack (for example a T2 ticket proposal), and a
// cancelled row is terminal: both are acknowledged no-ops. A row already in
// updated state is a same-decision redelivery and is also a no-op.
func (h *Handlers) HandleUpdate(ctx context.Context, msg outbox.Message) error {
	payload, err := parseApprovalUpdatePayload(msg.Payload)
	if err != nil {
		return err
	}

	row, found, err := h.store.Get(ctx, h.queryer, payload.ProposalID, approval.ChannelSlack)
	if err != nil {
		return err
	}
	if !found || row.State == StateCancelled || row.State == StateUpdated {
		h.logger.Debug("approval/slack: approval update has nothing to do",
			"proposal_id", payload.ProposalID, "found", found, "state", row.State)
		return nil
	}
	channel, ts, err := ParseExternalRef(row.ExternalRef)
	if err != nil {
		return err
	}

	data, err := h.source.ApprovalMessage(ctx, payload.ProposalID)
	if err != nil {
		return err
	}
	approver, err := h.approverName(ctx, payload.ApproverID)
	if err != nil {
		return err
	}
	opts := h.buildOptions()
	message, err := buildDecidedMessage(data, opts, decidedNotice{
		Decision:      payload.Decision,
		Source:        payload.Source,
		Approver:      approver,
		DecidedAt:     payload.DecidedAt,
		InfoRequested: payload.InfoRequested,
		Reason:        payload.Reason,
	})
	if err != nil {
		return fmt.Errorf("approval/slack: build decided message for %s: %w",
			payload.ProposalID, err)
	}

	if err := h.client.UpdateMessage(ctx, channel, ts, message); err != nil {
		if isSlackMessageGone(err) {
			// The original post is gone; cancellation is the only safe
			// terminal state and no retry can succeed.
			if _, markErr := h.store.MarkCancelled(ctx, h.queryer,
				payload.ProposalID, approval.ChannelSlack); markErr != nil {
				return markErr
			}
			h.logger.Warn("approval/slack: decided update target missing; marked cancelled",
				"proposal_id", payload.ProposalID)
			return nil
		}
		return fmt.Errorf("approval/slack: update decided message for %s: %w",
			payload.ProposalID, err)
	}

	// Re-read after the Slack call: cancellation is terminal (spec section
	// 6.2). If a cancel won the race, re-apply the withdrawn state instead of
	// leaving the decided message live.
	latest, found, err := h.store.Get(ctx, h.queryer, payload.ProposalID, approval.ChannelSlack)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	switch latest.State {
	case StateCancelled:
		withdrawn, err := buildWithdrawnMessage(data, opts, "approval cancelled")
		if err != nil {
			return fmt.Errorf("approval/slack: build withdrawn message for %s: %w",
				payload.ProposalID, err)
		}
		return h.updateRef(ctx, latest.ExternalRef, withdrawn)
	case StatePosted:
		changed, err := h.store.MarkUpdated(ctx, h.queryer,
			payload.ProposalID, approval.ChannelSlack)
		if err != nil {
			return err
		}
		if !changed {
			// A cancel moved the row between the re-read and the mark.
			current, found, err := h.store.Get(ctx, h.queryer,
				payload.ProposalID, approval.ChannelSlack)
			if err != nil {
				return err
			}
			if found && current.State == StateCancelled {
				withdrawn, buildErr := buildWithdrawnMessage(data, opts, "approval cancelled")
				if buildErr != nil {
					return fmt.Errorf("approval/slack: build withdrawn message for %s: %w",
						payload.ProposalID, buildErr)
				}
				return h.updateRef(ctx, current.ExternalRef, withdrawn)
			}
		}
	}
	h.logger.Info("approval/slack: updated decided message",
		"proposal_id", payload.ProposalID, "decision", string(payload.Decision))
	return nil
}

// HandleCancel delivers one approval.cancel row. Absent and already-cancelled
// rows are acknowledged no-ops. Cancellation is terminal: if an update won the
// race before the mark, the withdrawn message is re-applied.
func (h *Handlers) HandleCancel(ctx context.Context, msg outbox.Message) error {
	payload, err := parseApprovalCancelPayload(msg.Payload)
	if err != nil {
		return err
	}

	row, found, err := h.store.Get(ctx, h.queryer, payload.ProposalID, approval.ChannelSlack)
	if err != nil {
		return err
	}
	if !found || row.State == StateCancelled {
		h.logger.Debug("approval/slack: approval cancel has nothing to do",
			"proposal_id", payload.ProposalID, "found", found, "state", row.State)
		return nil
	}
	channel, ts, err := ParseExternalRef(row.ExternalRef)
	if err != nil {
		return err
	}
	data, err := h.source.ApprovalMessage(ctx, payload.ProposalID)
	if err != nil {
		return err
	}
	opts := h.buildOptions()
	withdrawn, err := buildWithdrawnMessage(data, opts, payload.Reason)
	if err != nil {
		return fmt.Errorf("approval/slack: build withdrawn message for %s: %w",
			payload.ProposalID, err)
	}
	if err := h.client.UpdateMessage(ctx, channel, ts, withdrawn); err != nil {
		if isSlackMessageGone(err) {
			if _, markErr := h.store.MarkCancelled(ctx, h.queryer,
				payload.ProposalID, approval.ChannelSlack); markErr != nil {
				return markErr
			}
			h.logger.Warn("approval/slack: cancel target missing; marked cancelled",
				"proposal_id", payload.ProposalID)
			return nil
		}
		return fmt.Errorf("approval/slack: withdraw approval message for %s: %w",
			payload.ProposalID, err)
	}

	// Re-read after the Slack call: if an update already marked the row
	// updated, re-apply the withdrawn message before recording cancellation.
	latest, found, err := h.store.Get(ctx, h.queryer, payload.ProposalID, approval.ChannelSlack)
	if err != nil {
		return err
	}
	if found && latest.State == StateUpdated {
		if err := h.updateRef(ctx, latest.ExternalRef, withdrawn); err != nil {
			return fmt.Errorf("approval/slack: re-apply withdrawal for %s: %w",
				payload.ProposalID, err)
		}
	}
	if _, err := h.store.MarkCancelled(ctx, h.queryer,
		payload.ProposalID, approval.ChannelSlack); err != nil {
		return err
	}
	h.logger.Info("approval/slack: cancelled approval message",
		"proposal_id", payload.ProposalID)
	return nil
}

// ParseExternalRef splits the frozen approval_messages.external_ref form
// "<channel_id>:<ts>". Exactly one colon is required and both halves must be
// non-empty; the parser never guesses which side is which.
func ParseExternalRef(ref string) (channel, ts string, err error) {
	if ref == "" || strings.TrimSpace(ref) != ref {
		return "", "", fmt.Errorf("%w: external ref is empty or padded", ErrInvalidMessage)
	}
	if strings.Count(ref, ":") != 1 {
		return "", "", fmt.Errorf(
			"%w: external ref %q must contain exactly one colon", ErrInvalidMessage, ref)
	}
	channel, ts, _ = strings.Cut(ref, ":")
	if channel == "" || ts == "" {
		return "", "", fmt.Errorf(
			"%w: external ref %q must have non-empty channel and ts", ErrInvalidMessage, ref)
	}
	if strings.TrimSpace(channel) != channel || strings.TrimSpace(ts) != ts {
		return "", "", fmt.Errorf(
			"%w: external ref %q has padded halves", ErrInvalidMessage, ref)
	}
	return channel, ts, nil
}

// recordCancelled inserts the posted row with the new ref (idempotent on the
// primary key) and records the terminal cancelled state. A conflicting row is
// still transitioned, because the proposal is not pending any more.
func (h *Handlers) recordCancelled(ctx context.Context, proposalID, ref string) error {
	if _, err := h.store.InsertPosted(ctx, h.queryer, proposalID,
		approval.ChannelSlack, ref); err != nil {
		return err
	}
	if _, err := h.store.MarkCancelled(ctx, h.queryer,
		proposalID, approval.ChannelSlack); err != nil {
		return err
	}
	return nil
}

// updateRef applies msg to the message named by ref. A Slack
// message_not_found/cant_update_message is treated as terminal: there is no
// retry that can succeed.
func (h *Handlers) updateRef(ctx context.Context, ref string, msg OutgoingMessage) error {
	channel, ts, err := ParseExternalRef(ref)
	if err != nil {
		return err
	}
	if err := h.client.UpdateMessage(ctx, channel, ts, msg); err != nil {
		if isSlackMessageGone(err) {
			return nil
		}
		return err
	}
	return nil
}

func (h *Handlers) approverName(ctx context.Context, approverID string) (string, error) {
	if h.approvers == nil {
		return "Approver " + shortID(approverID), nil
	}
	name, err := h.approvers.ApproverDisplayName(ctx, approverID)
	if err != nil {
		return "", err
	}
	return name, nil
}

func (h *Handlers) buildOptions() BuildOptions {
	return BuildOptions{
		ChannelID:       h.channelID,
		CaseFileBaseURL: h.caseFileBaseURL,
		Now:             h.clock.Now().UTC(),
	}
}

func isSlackMessageGone(err error) bool {
	var apiErr ErrSlackAPI
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.Code {
	case "message_not_found", "cant_update_message":
		return true
	default:
		return false
	}
}

// decidedNotice is the render input for an in-place decided message.
type decidedNotice struct {
	Decision      approval.Decision
	Source        string
	Approver      string
	DecidedAt     time.Time
	InfoRequested bool
	Reason        string
}

// buildDecidedMessage reuses the frozen request blocks and turns them into a
// terminal decided message: the header and fallback reflect the outcome, a
// decision section is inserted after the header, and the actions block is
// removed so no stale button remains.
func buildDecidedMessage(data MessageData, opts BuildOptions, notice decidedNotice) (OutgoingMessage, error) {
	base, err := BuildMessage(data, opts)
	if err != nil {
		return OutgoingMessage{}, err
	}
	label := "Rejected"
	header := "Approval rejected"
	fallback := "Approval rejected"
	if notice.InfoRequested {
		label = "Rejected — information requested"
		header = "Info requested"
		fallback = "Info requested"
	} else if notice.Decision == approval.DecisionApproved {
		label = "Approved"
		header = "Approval approved"
		fallback = "Approval approved"
	}

	lines := []string{"*Decision* " + label}
	if notice.Approver != "" {
		lines = append(lines, "*Approver* "+capEscaped(sanitize(notice.Approver), 200))
	}
	if !notice.DecidedAt.IsZero() {
		lines = append(lines, "*Decided* "+notice.DecidedAt.UTC().Format(time.RFC3339))
	}
	if notice.Source != "" {
		lines = append(lines, "*Source* "+capEscaped(sanitize(notice.Source), 64))
	}
	if notice.Reason != "" {
		lines = append(lines, "*Reason* "+capEscaped(sanitize(notice.Reason), 500))
	}
	decisionBlock := Block{
		Type: "section",
		Text: &TextObject{Type: "mrkdwn", Text: strings.Join(lines, "\n")},
	}

	blocks := replaceHeader(base.Blocks, header+" · "+shortID(data.CaseID))
	blocks = insertAfterHeader(blocks, decisionBlock)
	blocks = removeActions(blocks)
	if err := validateRendered(blocks); err != nil {
		return OutgoingMessage{}, err
	}
	base.Blocks = blocks
	base.Text = strings.Replace(base.Text, "Approval required", fallback, 1)
	base.ClientMsgID = ""
	return base, nil
}

// buildWithdrawnMessage reuses the frozen request blocks and turns them into a
// withdrawn message: no buttons, a status section with the bounded/escaped
// reason, and a fallback that no longer says approval is required.
func buildWithdrawnMessage(data MessageData, opts BuildOptions, reason string) (OutgoingMessage, error) {
	base, err := BuildMessage(data, opts)
	if err != nil {
		return OutgoingMessage{}, err
	}
	lines := []string{"*Status* Withdrawn"}
	if strings.TrimSpace(reason) != "" {
		lines = append(lines, "*Reason* "+capEscaped(sanitize(reason), 500))
	}
	statusBlock := Block{
		Type: "section",
		Text: &TextObject{Type: "mrkdwn", Text: strings.Join(lines, "\n")},
	}
	blocks := replaceHeader(base.Blocks, "Approval withdrawn · "+shortID(data.CaseID))
	blocks = insertAfterHeader(blocks, statusBlock)
	blocks = removeActions(blocks)
	if err := validateRendered(blocks); err != nil {
		return OutgoingMessage{}, err
	}
	base.Blocks = blocks
	base.Text = strings.Replace(base.Text, "Approval required", "Approval withdrawn", 1)
	base.ClientMsgID = ""
	return base, nil
}

func replaceHeader(blocks []Block, text string) []Block {
	for i := range blocks {
		if blocks[i].Type != "header" {
			continue
		}
		blocks[i].Text = &TextObject{Type: "plain_text", Text: text}
		return blocks
	}
	return blocks
}

func insertAfterHeader(blocks []Block, block Block) []Block {
	out := make([]Block, 0, len(blocks)+1)
	inserted := false
	for _, existing := range blocks {
		out = append(out, existing)
		if !inserted && existing.Type == "header" {
			out = append(out, block)
			inserted = true
		}
	}
	if !inserted {
		out = append(out, block)
	}
	return out
}

func removeActions(blocks []Block) []Block {
	out := blocks[:0]
	for _, block := range blocks {
		if block.Type == "actions" {
			continue
		}
		out = append(out, block)
	}
	return out
}

// Wire payloads are decoded strictly so a producer contract drift surfaces as
// a typed error instead of a partially rendered message.

func parseApprovalRequestPayload(raw json.RawMessage) (ApprovalRequestPayload, error) {
	var payload ApprovalRequestPayload
	if err := decodeStrictPayload(raw, &payload); err != nil {
		return ApprovalRequestPayload{}, err
	}
	if !canonicalUUIDPattern.MatchString(payload.ProposalID) {
		return ApprovalRequestPayload{}, fmt.Errorf(
			"%w: proposal id %q is not a canonical lowercase UUID",
			ErrInvalidPayload, payload.ProposalID)
	}
	if payload.Channel == "" || strings.TrimSpace(payload.Channel) != payload.Channel {
		return ApprovalRequestPayload{}, fmt.Errorf(
			"%w: channel is required and must not be padded", ErrInvalidPayload)
	}
	if payload.Channel != approval.ChannelSlack {
		// Non-slack channels are acknowledged without validating Slack-only
		// routing fields; the ticket adapter owns its own payload rules.
		return payload, nil
	}
	if !canonicalUUIDPattern.MatchString(payload.CaseID) {
		return ApprovalRequestPayload{}, fmt.Errorf(
			"%w: case id %q is not a canonical lowercase UUID",
			ErrInvalidPayload, payload.CaseID)
	}
	switch payload.Tier {
	case "T1", "T2":
	default:
		return ApprovalRequestPayload{}, fmt.Errorf(
			"%w: tier %q is not a human approval tier", ErrInvalidPayload, payload.Tier)
	}
	if payload.RequestedAt != "" {
		if _, err := time.Parse(time.RFC3339Nano, payload.RequestedAt); err != nil {
			return ApprovalRequestPayload{}, fmt.Errorf(
				"%w: requested_at is not RFC3339Nano: %v", ErrInvalidPayload, err)
		}
	}
	return payload, nil
}

type parsedApprovalUpdate struct {
	ProposalID    string
	Decision      approval.Decision
	Source        string
	ApproverID    string
	DecidedAt     time.Time
	InfoRequested bool
	Reason        string
}

func parseApprovalUpdatePayload(raw json.RawMessage) (parsedApprovalUpdate, error) {
	var payload ApprovalUpdatePayload
	if err := decodeStrictPayload(raw, &payload); err != nil {
		return parsedApprovalUpdate{}, err
	}
	if !canonicalUUIDPattern.MatchString(payload.ProposalID) {
		return parsedApprovalUpdate{}, fmt.Errorf(
			"%w: proposal id %q is not a canonical lowercase UUID",
			ErrInvalidPayload, payload.ProposalID)
	}
	if !canonicalUUIDPattern.MatchString(payload.CaseID) {
		return parsedApprovalUpdate{}, fmt.Errorf(
			"%w: case id %q is not a canonical lowercase UUID",
			ErrInvalidPayload, payload.CaseID)
	}
	if !canonicalUUIDPattern.MatchString(payload.ApproverID) {
		return parsedApprovalUpdate{}, fmt.Errorf(
			"%w: approver id %q is not a canonical lowercase UUID",
			ErrInvalidPayload, payload.ApproverID)
	}
	decision := approval.Decision(payload.Decision)
	if !decision.Valid() {
		return parsedApprovalUpdate{}, fmt.Errorf(
			"%w: decision %q is not approved or rejected",
			ErrInvalidPayload, payload.Decision)
	}
	switch decision {
	case approval.DecisionApproved:
		if payload.Status != "approved" {
			return parsedApprovalUpdate{}, fmt.Errorf(
				"%w: status %q does not match decision approved",
				ErrInvalidPayload, payload.Status)
		}
	case approval.DecisionRejected:
		if payload.Status != "rejected" {
			return parsedApprovalUpdate{}, fmt.Errorf(
				"%w: status %q does not match decision rejected",
				ErrInvalidPayload, payload.Status)
		}
	}
	if payload.InfoRequested && decision != approval.DecisionRejected {
		return parsedApprovalUpdate{}, fmt.Errorf(
			"%w: info_requested requires a rejected decision", ErrInvalidPayload)
	}
	if payload.Source == "" || strings.TrimSpace(payload.Source) != payload.Source {
		return parsedApprovalUpdate{}, fmt.Errorf(
			"%w: source is required and must not be padded", ErrInvalidPayload)
	}
	if payload.DecidedAt == "" {
		return parsedApprovalUpdate{}, fmt.Errorf(
			"%w: decided_at is required", ErrInvalidPayload)
	}
	decidedAt, err := time.Parse(time.RFC3339Nano, payload.DecidedAt)
	if err != nil {
		return parsedApprovalUpdate{}, fmt.Errorf(
			"%w: decided_at is not RFC3339Nano: %v", ErrInvalidPayload, err)
	}
	return parsedApprovalUpdate{
		ProposalID:    payload.ProposalID,
		Decision:      decision,
		Source:        payload.Source,
		ApproverID:    payload.ApproverID,
		DecidedAt:     decidedAt.UTC(),
		InfoRequested: payload.InfoRequested,
		Reason:        payload.Reason,
	}, nil
}

func parseApprovalCancelPayload(raw json.RawMessage) (ApprovalCancelPayload, error) {
	var payload ApprovalCancelPayload
	if err := decodeStrictPayload(raw, &payload); err != nil {
		return ApprovalCancelPayload{}, err
	}
	if !canonicalUUIDPattern.MatchString(payload.ProposalID) {
		return ApprovalCancelPayload{}, fmt.Errorf(
			"%w: proposal id %q is not a canonical lowercase UUID",
			ErrInvalidPayload, payload.ProposalID)
	}
	if !canonicalUUIDPattern.MatchString(payload.CaseID) {
		return ApprovalCancelPayload{}, fmt.Errorf(
			"%w: case id %q is not a canonical lowercase UUID",
			ErrInvalidPayload, payload.CaseID)
	}
	return payload, nil
}

func decodeStrictPayload(raw json.RawMessage, dst any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return fmt.Errorf("%w: payload is empty", ErrInvalidPayload)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("%w: decode payload: %v", ErrInvalidPayload, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: payload has trailing data", ErrInvalidPayload)
	}
	return nil
}
