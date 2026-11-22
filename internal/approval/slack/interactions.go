package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/approval"
	"github.com/TamasGorgics/recon-agent/internal/approval/state"
	"github.com/TamasGorgics/recon-agent/internal/audit"
	casestate "github.com/TamasGorgics/recon-agent/internal/cases/state"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/telemetry"
)

// Interaction endpoint constants. The action ids are shared with BuildMessage;
// the source is stored as approvals.channel and in the decision audit.
const (
	// InteractionSource is the approval source for every Slack callback.
	InteractionSource = approval.ChannelSlack
	// InteractionFormContentType is the only accepted request media type.
	InteractionFormContentType = "application/x-www-form-urlencoded"
	// MaxInteractionBodyBytes caps the raw form body read by the handler. The
	// signature verifier normally enforces the same 1 MiB cap first.
	MaxInteractionBodyBytes = 1 << 20
	// MaxInteractionPayloadBytes caps the decoded JSON payload string.
	MaxInteractionPayloadBytes = 64 << 10
	// DefaultEphemeralTimeout bounds the best-effort chat.postEphemeral call.
	DefaultEphemeralTimeout = 2 * time.Second

	maxSlackUserIDRunes       = 128
	maxSlackChannelRunes      = 128
	maxSlackTimestampRunes    = 64
	interactionEntityProposal = "proposal"

	interactionCodeInvalidRequest  = "invalid_request"
	interactionCodePayloadTooLarge = "payload_too_large"
	interactionCodeInternal        = "internal_error"
)

// Interaction denial reasons recorded in the approval.denied audit entry.
const (
	denialReasonUnknownUser = "unknown_user"
	denialReasonInactive    = "inactive"
	denialReasonRole        = "role"
)

// Ephemeral texts. They are deliberately generic and never include internal
// roles, proposal contents, or secrets.
const (
	textDenied           = "You are not authorized to approve this proposal."
	textOutOfDate        = "This approval is no longer active."
	textCoMaker          = "You contributed to this proposal and cannot approve it."
	textDecisionConflict = "This proposal already has a conflicting decision."
	textApproved         = "Approval recorded."
	textRejected         = "Rejection recorded."
	textInfoRequested    = "Information requested."
)

// InteractionDecisionService is the approval surface the callback needs. It is
// satisfied by *approval.Service. Splitting the methods lets tests inject an
// identity or decision failure without a database.
type InteractionDecisionService interface {
	ApproverBySlackUserID(ctx context.Context, slackUserID string) (approval.Approver, error)
	Decide(ctx context.Context, proposalID string, d approval.Decision, approverID, source string) (approval.DecisionResult, error)
	DecideWithOptions(ctx context.Context, proposalID string, d approval.Decision, approverID, source string, opts approval.DecisionOptions) (approval.DecisionResult, error)
}

// InteractionOptions is the dependency set for the Slack interaction endpoint.
// DB, Queryer, Decisions, and Client are required. Store defaults to the
// stateless repository, Clock to clock.Real, and Logger to slog.Default.
type InteractionOptions struct {
	// DB starts the short approval.denied audit transaction. It is usually the
	// same recon_app pool as Queryer.
	DB db.Beginner
	// Queryer runs the single-statement message and proposal reads.
	Queryer Queryer
	// Store reads the approval_messages row. Nil means &Store{}.
	Store *Store
	// Decisions resolves the Slack identity and records approve/reject
	// decisions through the existing approval service.
	Decisions InteractionDecisionService
	// Client posts best-effort ephemeral acknowledgements and denials.
	Client *Client
	// Clock supplies the audit timestamp and keeps tests deterministic.
	Clock clock.Clock
	// Logger receives rejection diagnostics without payloads or secrets.
	Logger *slog.Logger
	// EphemeralTimeout bounds chat.postEphemeral; zero selects 2s.
	EphemeralTimeout time.Duration
}

// InteractionHandler serves POST /v1/slack/interactions after the signature
// middleware has verified and restored the raw form body. It never trusts an
// approver id, role, decision, or amount from Slack: identity comes from the
// approvers table and the decision from the clicked action id.
type InteractionHandler struct {
	db               db.Beginner
	queryer          Queryer
	store            *Store
	decisions        InteractionDecisionService
	client           *Client
	clock            clock.Clock
	logger           *slog.Logger
	ephemeralTimeout time.Duration
}

// NewInteractionHandler validates its dependencies and applies defaults.
func NewInteractionHandler(opts InteractionOptions) (*InteractionHandler, error) {
	if opts.DB == nil {
		return nil, fmt.Errorf("%w: database is required", ErrInvalidHandler)
	}
	if err := validateQueryer(opts.Queryer); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidHandler, err)
	}
	if opts.Decisions == nil {
		return nil, fmt.Errorf("%w: decision service is required", ErrInvalidHandler)
	}
	if opts.Client == nil {
		return nil, fmt.Errorf("%w: slack client is required", ErrInvalidHandler)
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
	timeout := opts.EphemeralTimeout
	if timeout <= 0 {
		timeout = DefaultEphemeralTimeout
	}
	return &InteractionHandler{
		db:               opts.DB,
		queryer:          opts.Queryer,
		store:            store,
		decisions:        opts.Decisions,
		client:           opts.Client,
		clock:            clk,
		logger:           logger,
		ephemeralTimeout: timeout,
	}, nil
}

// interaction is the validated, canonical projection of one Slack payload.
type interaction struct {
	proposalID  string
	version     int
	actionID    string
	slackUserID string
	channelID   string
	messageTS   string
}

func (i interaction) decision() approval.Decision {
	if i.actionID == ActionReject {
		return approval.DecisionRejected
	}
	return approval.DecisionApproved
}

func (i interaction) actionName() string {
	switch i.actionID {
	case ActionApprove:
		return "approve"
	case ActionReject:
		return "reject"
	case ActionRequestInfo:
		return "request_info"
	default:
		return ""
	}
}

// ServeHTTP implements the interaction endpoint. Authentication has already
// happened in the outer signature middleware. Every authenticated outcome that
// is a business refusal is a 200 with an ephemeral message, so Slack stops
// retrying; 400 is reserved for malformed input and 500 for internal failure.
func (h *InteractionHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.writeError(w, http.StatusMethodNotAllowed, interactionCodeInvalidRequest,
			"the method is not allowed for this resource")
		return
	}
	in, parseErr := parseInteractionRequest(w, r)
	if parseErr != nil {
		h.writeError(w, parseErr.status, parseErr.code, parseErr.message)
		return
	}

	approver, err := h.decisions.ApproverBySlackUserID(r.Context(), in.slackUserID)
	if err != nil {
		if reason, denied := denialReason(err); denied {
			h.deny(w, r, in, reason)
			return
		}
		h.logFailure(r, "slack interaction identity lookup failed", err)
		h.writeError(w, http.StatusInternalServerError, interactionCodeInternal,
			"the server could not complete the request")
		return
	}

	if active, err := h.stillActive(r, in); err != nil {
		h.logFailure(r, "slack interaction state lookup failed", err)
		h.writeError(w, http.StatusInternalServerError, interactionCodeInternal,
			"the server could not complete the request")
		return
	} else if !active {
		h.ephemeral(r, in, textOutOfDate)
		h.writeOK(w)
		return
	}

	// Request-info is a rejection, but it takes the additive options path so
	// the lifecycle hook moves the case to waiting_info instead of consuming a
	// retry. The stale checks above are shared with approve/reject.
	var result approval.DecisionResult
	if in.actionID == ActionRequestInfo {
		result, err = h.decisions.DecideWithOptions(r.Context(), in.proposalID,
			approval.DecisionRejected, approver.ID, InteractionSource,
			approval.DecisionOptions{InfoRequested: true})
	} else {
		result, err = h.decisions.Decide(r.Context(), in.proposalID,
			in.decision(), approver.ID, InteractionSource)
	}
	if err != nil {
		switch {
		case errors.Is(err, approval.ErrCoMaker):
			h.ephemeral(r, in, textCoMaker)
		case errors.Is(err, approval.ErrDecisionConflict):
			h.ephemeral(r, in, textDecisionConflict)
		case errors.Is(err, approval.ErrNotPending),
			errors.Is(err, approval.ErrExpired),
			errors.Is(err, approval.ErrProposalNotFound):
			h.ephemeral(r, in, textOutOfDate)
		case errors.Is(err, approval.ErrApproverNotFound),
			errors.Is(err, approval.ErrApproverInactive),
			errors.Is(err, approval.ErrApproverRole):
			// The identity changed between the lookup and the decision lock.
			// Treat it exactly like the pre-decision denial: one audit entry,
			// no proposal/case/approvals write.
			h.deny(w, r, in, denialReasonFor(err))
			return
		default:
			h.logFailure(r, "slack interaction decision failed", err)
			h.writeError(w, http.StatusInternalServerError, interactionCodeInternal,
				"the server could not complete the request")
			return
		}
		h.writeOK(w)
		return
	}

	// The decision transaction is committed. Slack availability must never
	// roll it back, so the acknowledgement is best effort.
	switch {
	case in.actionID == ActionRequestInfo:
		h.ephemeral(r, in, textInfoRequested)
	case result.Decision == approval.DecisionRejected:
		h.ephemeral(r, in, textRejected)
	default:
		h.ephemeral(r, in, textApproved)
	}
	h.writeOK(w)
}

// stillActive checks the stored message reference, the proposal version, the
// proposal status, and (for request-info) the linked case status. A stale
// click never reaches Decide. A database failure is returned as an error so
// the caller can answer 500; a missing row, invalid ref, or state mismatch is
// simply "not active".
func (h *InteractionHandler) stillActive(r *http.Request, in interaction) (bool, error) {
	message, err := h.store.GetRequired(r.Context(), h.queryer, in.proposalID, approval.ChannelSlack)
	if err != nil {
		if errors.Is(err, ErrMessageNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("read approval message: %w", err)
	}
	channel, ts, err := ParseExternalRef(message.ExternalRef)
	if err != nil {
		h.logger.Warn("slack interaction stored ref is invalid",
			slog.String("trace_id", telemetry.TraceID(r.Context())))
		return false, nil
	}
	if message.State != StatePosted || channel != in.channelID || ts != in.messageTS {
		return false, nil
	}

	var (
		version    int
		status     string
		caseStatus string
	)
	err = h.queryer.QueryRow(r.Context(),
		`SELECT p.version, p.status, c.status
		   FROM proposals p
		   JOIN cases c ON c.id = p.case_id
		  WHERE p.id = $1::uuid`, in.proposalID).
		Scan(&version, &status, &caseStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read proposal state: %w", err)
	}
	if version != in.version || status != string(state.StatusPendingApproval) {
		return false, nil
	}
	if in.actionID == ActionRequestInfo {
		// Request-info runs the case lifecycle hook. A case that is no longer
		// proposal_pending (or waiting_info for an idempotent retry) would make
		// RequestInfo return an illegal-transition error and roll the whole
		// decision back into a 500. Treat it as a stale click instead.
		switch caseStatus {
		case string(casestate.StatusProposalPending), string(casestate.StatusWaitingInfo):
			return true, nil
		default:
			return false, nil
		}
	}
	return true, nil
}

// deny writes at most one approval.denied audit entry per (proposal, Slack
// user, action), then posts a best-effort ephemeral denial. A signed Slack
// retry reuses the stored denial instead of appending a duplicate. An audit
// failure is a 500 (Slack retries); ephemeral availability never affects the
// audit.
func (h *InteractionHandler) deny(w http.ResponseWriter, r *http.Request, in interaction, reason string) {
	if err := h.appendDenied(r.Context(), in, reason); err != nil {
		h.logFailure(r, "slack interaction denial audit failed", err)
		h.writeError(w, http.StatusInternalServerError, interactionCodeInternal,
			"the server could not complete the request")
		return
	}
	h.ephemeral(r, in, textDenied)
	h.writeOK(w)
}

func (h *InteractionHandler) appendDenied(ctx context.Context, in interaction, reason string) error {
	actor, err := audit.HumanActor(in.slackUserID)
	if err != nil {
		return fmt.Errorf("slack interaction denied actor: %w", err)
	}
	timestamp := h.clock.Now().UTC()
	// Only an active OTel span may become the persisted audit trace ID; the
	// HTTP correlation key is not trusted as a persistence value.
	var traceID *string
	if value := strings.TrimSpace(telemetry.SpanTraceID(ctx)); value != "" {
		traceID = &value
	}
	// A signed Slack retry replays the same (proposal, user, action) denial.
	// Serialize the check-and-append for that key and skip the append when an
	// identical denial already exists, so a retry leaves exactly one audit row
	// while a different user/action still gets its own denial.
	lockKey := approval.ActionApprovalDenied + ":" + in.proposalID + ":" +
		in.slackUserID + ":" + in.actionName()
	return db.WithTx(ctx, h.db, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, lockKey); err != nil {
			return fmt.Errorf("slack interaction denied audit lock: %w", err)
		}
		var exists bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
			    SELECT 1
			      FROM audit_log
			     WHERE action = $1
			       AND entity_type = $2
			       AND entity_id = $3
			       AND actor_type = 'human'
			       AND actor_id = $4
			       AND after->>'denied' = 'true'
			       AND after->>'slack_user_id' = $5
			       AND after->>'action' = $6
			)`,
			approval.ActionApprovalDenied, interactionEntityProposal, in.proposalID,
			actor.ID(), in.slackUserID, in.actionName()).Scan(&exists); err != nil {
			return fmt.Errorf("slack interaction denied audit lookup: %w", err)
		}
		if exists {
			return nil
		}

		_, err := audit.Append(ctx, tx, audit.Entry{
			Actor:      actor,
			Action:     approval.ActionApprovalDenied,
			EntityType: interactionEntityProposal,
			EntityID:   in.proposalID,
			After: map[string]any{
				"denied":        true,
				"slack_user_id": in.slackUserID,
				"action":        in.actionName(),
				"reason":        reason,
			},
			TraceID:   traceID,
			Timestamp: timestamp,
		})
		if err != nil {
			return fmt.Errorf("slack interaction denied audit: %w", err)
		}
		return nil
	})
}

// ephemeral posts a best-effort acknowledgement with a short deadline. A
// failure is logged and otherwise ignored.
func (h *InteractionHandler) ephemeral(r *http.Request, in interaction, text string) {
	ctx, cancel := context.WithTimeout(r.Context(), h.ephemeralTimeout)
	defer cancel()
	if err := h.client.PostEphemeral(ctx, in.channelID, in.slackUserID, text); err != nil {
		h.logger.Warn("slack interaction ephemeral failed",
			slog.String("trace_id", telemetry.TraceID(r.Context())),
			slog.String("error", err.Error()),
		)
	}
}

// logFailure records the server-side cause with correlation ids only.
func (h *InteractionHandler) logFailure(r *http.Request, message string, err error) {
	h.logger.Error(message,
		slog.String("trace_id", telemetry.TraceID(r.Context())),
		slog.String("error", err.Error()),
	)
}

// interactionClientError is a malformed-request failure safe to expose.
type interactionClientError struct {
	status  int
	code    string
	message string
}

func newInteractionClientError(status int, code, message string) *interactionClientError {
	return &interactionClientError{status: status, code: code, message: message}
}

// parseInteractionRequest decodes the form body and the block_actions payload.
// It never reads an identity, role, or decision from the JSON.
func parseInteractionRequest(w http.ResponseWriter, r *http.Request) (interaction, *interactionClientError) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, InteractionFormContentType) {
		return interaction{}, newInteractionClientError(http.StatusBadRequest,
			interactionCodeInvalidRequest, "content type must be application/x-www-form-urlencoded")
	}

	limited := http.MaxBytesReader(w, r.Body, MaxInteractionBodyBytes)
	defer func() { _ = limited.Close() }()
	body, err := io.ReadAll(limited)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return interaction{}, newInteractionClientError(http.StatusRequestEntityTooLarge,
				interactionCodePayloadTooLarge, "request body is too large")
		}
		return interaction{}, newInteractionClientError(http.StatusBadRequest,
			interactionCodeInvalidRequest, "request body could not be read")
	}

	payload, formErr := parseInteractionForm(string(body))
	if formErr != nil {
		return interaction{}, formErr
	}
	return parseInteractionPayload(payload)
}

type interactionAction struct {
	ActionID string          `json:"action_id"`
	Value    string          `json:"value"`
	Type     string          `json:"type,omitempty"`
	BlockID  string          `json:"block_id,omitempty"`
	ActionTS string          `json:"action_ts,omitempty"`
	Text     json.RawMessage `json:"text,omitempty"`
	// Style is echoed by Slack for a styled button ("primary" or "danger").
	// The handler never trusts it; accepting it keeps a real block_actions
	// payload valid under the strict decoder.
	Style string `json:"style,omitempty"`
}

// interactionIdentity accepts the Slack user/channel fields we never trust or
// read beyond id. Unknown fields are still rejected by DisallowUnknownFields;
// these are the documented envelope fields a real block_actions payload may
// carry.
type interactionIdentity struct {
	ID           string `json:"id"`
	Username     string `json:"username,omitempty"`
	Name         string `json:"name,omitempty"`
	TeamID       string `json:"team_id,omitempty"`
	Domain       string `json:"domain,omitempty"`
	EnterpriseID string `json:"enterprise_id,omitempty"`
	IsBot        *bool  `json:"is_bot,omitempty"`
	IsAppUser    *bool  `json:"is_app_user,omitempty"`
}

type interactionMessage struct {
	TS          string          `json:"ts"`
	Type        string          `json:"type,omitempty"`
	Subtype     string          `json:"subtype,omitempty"`
	Text        string          `json:"text,omitempty"`
	User        string          `json:"user,omitempty"`
	BotID       string          `json:"bot_id,omitempty"`
	Username    string          `json:"username,omitempty"`
	ThreadTS    string          `json:"thread_ts,omitempty"`
	Permalink   string          `json:"permalink,omitempty"`
	Blocks      json.RawMessage `json:"blocks,omitempty"`
	Attachments json.RawMessage `json:"attachments,omitempty"`
	Edited      json.RawMessage `json:"edited,omitempty"`
}

// interactionPayload is decoded strictly. The accepted-but-unused fields are
// Slack envelope data the handler never trusts; they are enumerated so a
// genuinely unknown field still fails the 400 path.
type interactionPayload struct {
	Type                string               `json:"type"`
	User                *interactionIdentity `json:"user"`
	Channel             *interactionIdentity `json:"channel"`
	Message             *interactionMessage  `json:"message"`
	Actions             []interactionAction  `json:"actions"`
	Team                json.RawMessage      `json:"team,omitempty"`
	APIAppID            string               `json:"api_app_id,omitempty"`
	Token               string               `json:"token,omitempty"`
	Container           json.RawMessage      `json:"container,omitempty"`
	TriggerID           string               `json:"trigger_id,omitempty"`
	ResponseURL         string               `json:"response_url,omitempty"`
	State               json.RawMessage      `json:"state,omitempty"`
	View                json.RawMessage      `json:"view,omitempty"`
	Enterprise          json.RawMessage      `json:"enterprise,omitempty"`
	IsEnterpriseInstall *bool                `json:"is_enterprise_install,omitempty"`
	WorkflowStep        json.RawMessage      `json:"workflow_step,omitempty"`
}

func parseInteractionPayload(raw []byte) (interaction, *interactionClientError) {
	invalid := func(message string) (interaction, *interactionClientError) {
		return interaction{}, newInteractionClientError(http.StatusBadRequest,
			interactionCodeInvalidRequest, message)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return invalid("payload is required")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var payload interactionPayload
	if err := decoder.Decode(&payload); err != nil {
		return invalid("payload is not valid JSON or contains an unknown field")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return invalid("payload must contain exactly one JSON value")
	}
	if payload.Type != "block_actions" {
		return invalid("payload type must be block_actions")
	}
	if len(payload.Actions) != 1 {
		return invalid("payload must contain exactly one action")
	}
	action := payload.Actions[0]
	switch action.ActionID {
	case ActionApprove, ActionReject, ActionRequestInfo:
	default:
		return invalid("payload action is not an approval action")
	}
	if payload.User == nil {
		return invalid("payload user.id is required")
	}
	if err := validateSlackToken(payload.User.ID, maxSlackUserIDRunes); err != nil {
		return invalid("payload user.id is invalid")
	}
	if payload.Channel == nil {
		return invalid("payload channel.id is required")
	}
	if err := validateSlackToken(payload.Channel.ID, maxSlackChannelRunes); err != nil {
		return invalid("payload channel.id is invalid")
	}
	if payload.Message == nil {
		return invalid("payload message.ts is required")
	}
	if err := validateSlackTimestamp(payload.Message.TS); err != nil {
		return invalid("payload message.ts is invalid")
	}
	proposalID, version, err := parseInteractionValue(action.Value)
	if err != nil {
		return invalid("payload action value is invalid")
	}
	return interaction{
		proposalID:  proposalID,
		version:     version,
		actionID:    action.ActionID,
		slackUserID: payload.User.ID,
		channelID:   payload.Channel.ID,
		messageTS:   payload.Message.TS,
	}, nil
}

// parseInteractionValue accepts exactly "<canonical-uuid>:<positive-version>".
func parseInteractionValue(value string) (string, int, error) {
	if value == "" || strings.TrimSpace(value) != value {
		return "", 0, errors.New("value is empty or padded")
	}
	if strings.Count(value, ":") != 1 {
		return "", 0, errors.New("value must contain exactly one colon")
	}
	proposalID, versionText, _ := strings.Cut(value, ":")
	if !canonicalUUIDPattern.MatchString(proposalID) {
		return "", 0, errors.New("proposal id is not a canonical lowercase UUID")
	}
	if versionText == "" || (len(versionText) > 1 && versionText[0] == '0') {
		return "", 0, errors.New("version is not canonical")
	}
	for i := 0; i < len(versionText); i++ {
		if versionText[i] < '0' || versionText[i] > '9' {
			return "", 0, errors.New("version is not numeric")
		}
	}
	version, err := strconv.Atoi(versionText)
	if err != nil || version <= 0 {
		return "", 0, errors.New("version must be a positive integer")
	}
	return proposalID, version, nil
}

// validateSlackToken accepts a bounded printable-ASCII token without spaces or
// colons. Slack user and channel ids have this shape; the audit actor and the
// stored external ref both rely on it.
func validateSlackToken(value string, maxRunes int) error {
	if value == "" || utf8.RuneCountInString(value) > maxRunes {
		return errors.New("token is empty or too long")
	}
	for _, r := range value {
		if r <= 0x20 || r == 0x7f || r > 0x7e || r == ':' {
			return errors.New("token contains an invalid character")
		}
	}
	return nil
}

// validateSlackTimestamp accepts a bounded decimal Slack timestamp with at most
// one dot, for example 1732041600.123456.
func validateSlackTimestamp(value string) error {
	if value == "" || len(value) > maxSlackTimestampRunes {
		return errors.New("timestamp is empty or too long")
	}
	dots := 0
	for i := 0; i < len(value); i++ {
		switch {
		case value[i] >= '0' && value[i] <= '9':
		case value[i] == '.':
			dots++
			if dots > 1 {
				return errors.New("timestamp has more than one dot")
			}
		default:
			return errors.New("timestamp contains an invalid character")
		}
	}
	if value[0] == '.' || value[len(value)-1] == '.' {
		return errors.New("timestamp has an empty segment")
	}
	return nil
}

// denialReason maps an identity error to the audit reason, or reports false
// when the failure is not an authorization outcome.
func denialReason(err error) (string, bool) {
	switch {
	case errors.Is(err, approval.ErrApproverNotFound):
		return denialReasonUnknownUser, true
	case errors.Is(err, approval.ErrApproverInactive):
		return denialReasonInactive, true
	case errors.Is(err, approval.ErrApproverRole):
		return denialReasonRole, true
	default:
		return "", false
	}
}

func denialReasonFor(err error) string {
	reason, _ := denialReason(err)
	return reason
}

// writeOK writes the minimal Slack acknowledgement body.
func (h *InteractionHandler) writeOK(w http.ResponseWriter) {
	h.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// writeError writes the shared error envelope shape. The request id is read
// from the response header set by the API middleware.
func (h *InteractionHandler) writeError(w http.ResponseWriter, status int, code, message string) {
	envelope := interactionErrorEnvelope{RequestID: w.Header().Get("X-Request-ID")}
	envelope.Error.Code = code
	envelope.Error.Message = message
	h.writeJSON(w, status, envelope)
}

type interactionErrorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	RequestID string `json:"request_id"`
}

func (h *InteractionHandler) writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
