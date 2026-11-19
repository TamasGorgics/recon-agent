package slack

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/TamasGorgics/recon-agent/internal/approval"
	"github.com/TamasGorgics/recon-agent/internal/clock"
)

// ChannelStore is the narrow approval_messages persistence surface the direct
// Channel path needs. Each method is a single statement; the Channel never
// opens a transaction around the Slack network call (FIN-22). *DBChannelStore
// is the production adapter; tests may supply an in-memory fake.
type ChannelStore interface {
	Get(ctx context.Context, proposalID, channel string) (Message, bool, error)
	InsertPosted(ctx context.Context, proposalID, channel, externalRef string) (bool, error)
}

// SlackApproverReader resolves a Slack user id to the internal approver
// identity. *approval.Service satisfies it. The Channel never trusts an
// approver id from the Slack payload: it always resolves the identity here.
type SlackApproverReader interface {
	ApproverBySlackUserID(ctx context.Context, slackUserID string) (approval.Approver, error)
}

// Channel implements approval.Channel for the Slack adapter. Production
// delivery normally runs through Handlers via the outbox; Channel exists for
// interface compatibility and for tests/callers that already have a hydrated
// request. It performs no signature verification and never touches the replay
// cache: the API route middleware owns authentication for callbacks.
type Channel struct {
	source          Source
	store           ChannelStore
	approvers       SlackApproverReader
	client          *Client
	channelID       string
	caseFileBaseURL string
	clock           clock.Clock
	logger          *slog.Logger
}

var _ approval.Channel = (*Channel)(nil)

// ChannelOptions is the Channel dependency set. Source, Store, Approvers, and
// Client are required; ChannelID and CaseFileBaseURL are validated as Slack
// configuration. Clock defaults to clock.Real and Logger to slog.Default.
type ChannelOptions struct {
	Source          Source
	Store           ChannelStore
	Approvers       SlackApproverReader
	Client          *Client
	ChannelID       string
	CaseFileBaseURL string
	Clock           clock.Clock
	Logger          *slog.Logger
}

// NewChannel validates the dependency set and returns a Slack channel.
func NewChannel(opts ChannelOptions) (*Channel, error) {
	if opts.Source == nil {
		return nil, fmt.Errorf("%w: source is required", ErrInvalidHandler)
	}
	if opts.Store == nil {
		return nil, fmt.Errorf("%w: store is required", ErrInvalidHandler)
	}
	if opts.Approvers == nil {
		return nil, fmt.Errorf("%w: approver reader is required", ErrInvalidHandler)
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
	clk := opts.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Channel{
		source:          opts.Source,
		store:           opts.Store,
		approvers:       opts.Approvers,
		client:          opts.Client,
		channelID:       opts.ChannelID,
		caseFileBaseURL: caseFileBaseURL,
		clock:           clk,
		logger:          logger,
	}, nil
}

// RequestApproval hydrates the message data, renders the Block Kit request,
// posts it synchronously, records the posted approval_messages row, and
// returns the "<channel>:<ts>" external reference.
//
// An existing row is the idempotency guard: a redelivery returns the stored
// reference without posting again. The Slack call happens between two short
// single-statement store calls; no database transaction is open (FIN-22).
// Production posting happens in the approval.request outbox handler; callers
// must not invoke this method while holding a transaction.
func (c *Channel) RequestApproval(ctx context.Context, req approval.ApprovalRequest) (string, error) {
	if c == nil {
		return "", fmt.Errorf("%w: nil channel", approval.ErrInvalidRequest)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !canonicalUUIDPattern.MatchString(req.ProposalID) {
		return "", fmt.Errorf("%w: proposal id %q is not a canonical lowercase UUID",
			approval.ErrInvalidRequest, req.ProposalID)
	}
	if req.Channel != approval.ChannelSlack {
		return "", fmt.Errorf("%w: channel %q is not slack",
			approval.ErrInvalidRequest, req.Channel)
	}

	existing, found, err := c.store.Get(ctx, req.ProposalID, approval.ChannelSlack)
	if err != nil {
		return "", err
	}
	if found {
		return existing.ExternalRef, nil
	}

	data, err := c.source.ApprovalMessage(ctx, req.ProposalID)
	if err != nil {
		return "", err
	}
	message, err := BuildMessage(data, BuildOptions{
		ChannelID:       c.channelID,
		CaseFileBaseURL: c.caseFileBaseURL,
		Now:             c.clock.Now().UTC(),
	})
	if err != nil {
		return "", fmt.Errorf("approval/slack: build request message for %s: %w",
			req.ProposalID, err)
	}
	ref, err := c.client.PostMessageRef(ctx, c.channelID, message)
	if err != nil {
		return "", fmt.Errorf("approval/slack: post approval request for %s: %w",
			req.ProposalID, err)
	}

	inserted, err := c.store.InsertPosted(ctx, req.ProposalID, approval.ChannelSlack, ref)
	if err != nil {
		return "", err
	}
	if !inserted {
		// Another delivery won the post/insert race. Return the stored ref so
		// callers converge on the winner; client_msg_id is the best-effort
		// duplicate guard described in Q-97.
		current, found, getErr := c.store.Get(ctx, req.ProposalID, approval.ChannelSlack)
		if getErr != nil {
			return "", getErr
		}
		if found {
			return current.ExternalRef, nil
		}
	}
	c.logger.Info("approval/slack: posted approval request",
		"proposal_id", req.ProposalID, "channel", c.channelID, "external_ref", ref)
	return ref, nil
}

// HandleCallback parses an already-authenticated form body into the existing
// block_actions interaction shape, resolves the Slack user through the
// approvers table, and returns the channel-neutral decision.
//
// It deliberately does not verify a Slack signature and never touches the
// replay cache: POST /v1/slack/interactions signature middleware is the
// production crypto boundary. CallbackDecision has no InfoRequested flag, so
// request-info is represented as DecisionRejected with Reason
// "info_requested"; a caller that needs the case lifecycle must use
// approval.Service.DecideWithOptions with DecisionOptions{InfoRequested: true}.
func (c *Channel) HandleCallback(ctx context.Context, req *http.Request) (approval.CallbackDecision, error) {
	if c == nil {
		return approval.CallbackDecision{}, fmt.Errorf("%w: nil channel", approval.ErrInvalidRequest)
	}
	if err := ctx.Err(); err != nil {
		return approval.CallbackDecision{}, err
	}
	in, err := parseCallbackInteraction(req)
	if err != nil {
		return approval.CallbackDecision{}, err
	}
	approver, err := c.approvers.ApproverBySlackUserID(ctx, in.slackUserID)
	if err != nil {
		return approval.CallbackDecision{}, fmt.Errorf(
			"approval/slack: resolve approver for Slack user: %w", err)
	}
	decision := in.decision()
	reason := ""
	if in.actionID == ActionRequestInfo {
		decision = approval.DecisionRejected
		reason = approval.ReasonInfoRequested
	}
	return approval.CallbackDecision{
		ProposalID: in.proposalID,
		Decision:   decision,
		ApproverID: approver.ID,
		Source:     approval.ChannelSlack,
		Reason:     reason,
	}, nil
}

// Cancel parses the frozen "<channel_id>:<ts>" reference and edits the Slack
// message to a generic withdrawn state with no buttons. It does not mark the
// approval_messages row: the approval.cancel outbox handler owns transport
// state. Slack failures are propagated so the caller/outbox can retry.
func (c *Channel) Cancel(ctx context.Context, externalRef string) error {
	if c == nil {
		return fmt.Errorf("%w: nil channel", approval.ErrInvalidRequest)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	channel, ts, err := ParseExternalRef(externalRef)
	if err != nil {
		return fmt.Errorf("%w: %w", approval.ErrInvalidRequest, err)
	}
	return c.client.UpdateMessage(ctx, channel, ts, genericWithdrawnMessage())
}

// DBChannelStore adapts the stateless Store to ChannelStore with a fixed
// queryer, normally the recon_app pool.
type DBChannelStore struct {
	queryer Queryer
	store   *Store
}

// NewDBChannelStore validates the queryer and returns a ChannelStore adapter.
func NewDBChannelStore(q Queryer) (*DBChannelStore, error) {
	if err := validateQueryer(q); err != nil {
		return nil, err
	}
	return &DBChannelStore{queryer: q, store: &Store{}}, nil
}

// Get loads one approval_messages row through the fixed queryer.
func (s *DBChannelStore) Get(ctx context.Context, proposalID, channel string) (Message, bool, error) {
	return s.store.Get(ctx, s.queryer, proposalID, channel)
}

// InsertPosted records a posted row through the fixed queryer.
func (s *DBChannelStore) InsertPosted(ctx context.Context, proposalID, channel, externalRef string) (bool, error) {
	return s.store.InsertPosted(ctx, s.queryer, proposalID, channel, externalRef)
}

func parseCallbackInteraction(req *http.Request) (interaction, error) {
	if req == nil {
		return interaction{}, callbackInvalid("request is required")
	}
	if req.Method != http.MethodPost {
		return interaction{}, callbackInvalid("method must be POST")
	}
	mediaType, _, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, InteractionFormContentType) {
		return interaction{}, callbackInvalid(
			"content type must be application/x-www-form-urlencoded")
	}
	if req.Body == nil {
		return interaction{}, callbackInvalid("request body is required")
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, MaxInteractionBodyBytes+1))
	if err != nil {
		return interaction{}, callbackInvalid("request body could not be read")
	}
	if len(body) > MaxInteractionBodyBytes {
		return interaction{}, callbackInvalid("request body is too large")
	}
	payload, formErr := parseInteractionForm(string(body))
	if formErr != nil {
		return interaction{}, callbackInvalid(formErr.message)
	}
	in, payloadErr := parseInteractionPayload(payload)
	if payloadErr != nil {
		return interaction{}, callbackInvalid(payloadErr.message)
	}
	return in, nil
}

// callbackInvalid wraps both the adapter payload sentinel and the frozen
// approval request sentinel, so callers can test either class.
func callbackInvalid(message string) error {
	return fmt.Errorf("%w: %w: %s", ErrInvalidPayload, approval.ErrInvalidRequest, message)
}

// parseInteractionForm validates the form envelope shared by the HTTP
// interaction handler and HandleCallback. Exactly one payload field is
// allowed, and unknown form fields are rejected per spec section 7.4.
func parseInteractionForm(raw string) ([]byte, *interactionClientError) {
	values, err := url.ParseQuery(raw)
	if err != nil {
		return nil, newInteractionClientError(http.StatusBadRequest,
			interactionCodeInvalidRequest, "request body is not a valid form")
	}
	for field := range values {
		if field != "payload" {
			return nil, newInteractionClientError(http.StatusBadRequest,
				interactionCodeInvalidRequest, "unknown form field")
		}
	}
	payloads, ok := values["payload"]
	if !ok || len(payloads) != 1 {
		return nil, newInteractionClientError(http.StatusBadRequest,
			interactionCodeInvalidRequest, "exactly one payload field is required")
	}
	if len(payloads[0]) > MaxInteractionPayloadBytes {
		return nil, newInteractionClientError(http.StatusBadRequest,
			interactionCodeInvalidRequest, "payload is too large")
	}
	return []byte(payloads[0]), nil
}

// genericWithdrawnMessage is the channel-only withdrawn state. It carries no
// proposal data and no buttons, so a Cancel direct call cannot leak content.
func genericWithdrawnMessage() OutgoingMessage {
	return OutgoingMessage{
		Text: "Approval withdrawn",
		Blocks: []Block{
			{
				Type: "header",
				Text: &TextObject{Type: "plain_text", Text: "Approval withdrawn"},
			},
			{
				Type: "section",
				Text: &TextObject{Type: "mrkdwn", Text: "*Status* Withdrawn"},
			},
		},
	}
}
