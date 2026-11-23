package approval

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/trace"

	"github.com/TamasGorgics/recon-agent/internal/approval/state"
	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/limits"
	"github.com/TamasGorgics/recon-agent/internal/policy"
	"github.com/TamasGorgics/recon-agent/internal/telemetry"
)

var loadedConfigHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Verifier is the deterministic verification step. The production adapter is
// VerifierAdapter, which wraps the M4-05 service; tests supply a fake.
type Verifier interface {
	Verify(ctx context.Context, proposalID string) (policy.Verification, error)
}

// Limits is the FIN-53 guard. *limits.Service satisfies it.
type Limits interface {
	Check(ctx context.Context, c limits.Candidate) (limits.Result, error)
}

// PolicyEngine is the pure evaluation surface the service needs. *policy.Engine
// satisfies it; tests substitute a deterministic fake.
type PolicyEngine interface {
	Evaluate(p policy.Proposal, v policy.Verification, f policy.Facts) policy.Decision
	ProposalTTL() time.Duration
}

// Policy couples the evaluator with the exact loaded document whose hash every
// decision carries. EnsureVersion persists that document before PersistDecision
// accepts the decision.
type Policy struct {
	Engine PolicyEngine
	Loaded policy.LoadedConfig
}

// ProposalClosedHook is the M4-10 lifecycle callback invoked inside Decide's
// transaction after a proposal transitions to rejected, or expires on a late
// decision. A nil hook is safe and leaves M4-11 behavior unchanged. M4-11 never
// imports the lifecycle package: cmd/api wires the concrete method value, and
// a hook error rolls the whole decision back.
type ProposalClosedHook func(ctx context.Context, tx pgx.Tx, proposalID, proposalStatus, reason string) error

// InfoRequestedHook is the M4-10 lifecycle callback invoked inside
// DecideWithOptions's transaction when DecisionOptions.InfoRequested is true.
// It replaces ProposalClosedHook for that path: the case moves to waiting_info
// instead of consuming an attempt or escalating. A nil hook fails closed
// before any write (ErrInfoRequestedUnsupported); a hook error rolls the whole
// decision back.
type InfoRequestedHook func(ctx context.Context, tx pgx.Tx, proposalID string) error

// Options is the approval service dependency set. Pool and State must use the
// recon_app role; TTL zero means the policy document's proposal_ttl_hours.
type Options struct {
	Pool     *pgxpool.Pool
	Clock    clock.Clock
	State    *state.Service
	Verifier Verifier
	Limits   Limits
	Policy   Policy
	TTL      time.Duration
	// OnProposalClosed is optional M4-10 lifecycle integration. It runs inside
	// the decision transaction after a rejection or a late-decision expiry; a
	// returned error aborts the decision.
	OnProposalClosed ProposalClosedHook
	// InfoRequestedHook is optional M4-10 lifecycle integration for
	// DecisionOptions.InfoRequested. It replaces OnProposalClosed on that path
	// and runs inside the decision transaction; a nil hook fails closed with
	// ErrInfoRequestedUnsupported before any write.
	InfoRequestedHook InfoRequestedHook
	// Telemetry is the optional tracing provider. A nil provider yields a
	// no-op tracer and changes no service behavior.
	Telemetry *telemetry.Provider
	// Observer optionally receives post-commit decision latency and bounded
	// failure codes. Nil disables observation and leaves behavior unchanged.
	Observer Observer
}

// Service routes proposed proposals through verification, policy, and limits,
// then records the decision, TTL, state transition, audit, and approval
// request in one transaction. It sends no network messages: T1/T2 produce one
// approval.request outbox row for M4-12.
type Service struct {
	pool        *pgxpool.Pool
	clock       clock.Clock
	state       *state.Service
	verifier    Verifier
	limits      Limits
	policy      Policy
	ttl         time.Duration
	systemActor audit.Actor
	expiryActor audit.Actor
	onClosed    ProposalClosedHook
	onInfo      InfoRequestedHook
	tracer      trace.Tracer
	observer    Observer
}

// NewService validates its dependencies and returns a read-mostly approval
// service. A nil dependency, a zero/negative effective TTL, or a missing loaded
// policy document is a construction error wrapping ErrInvalidRequest.
func NewService(opts Options) (*Service, error) {
	if opts.Pool == nil {
		return nil, fmt.Errorf("%w: nil pool", ErrInvalidRequest)
	}
	if isNilValue(opts.Clock) {
		return nil, fmt.Errorf("%w: nil clock", ErrInvalidRequest)
	}
	if opts.State == nil {
		return nil, fmt.Errorf("%w: nil state service", ErrInvalidRequest)
	}
	if isNilValue(opts.Verifier) {
		return nil, fmt.Errorf("%w: nil verifier", ErrInvalidRequest)
	}
	if isNilValue(opts.Limits) {
		return nil, fmt.Errorf("%w: nil limits", ErrInvalidRequest)
	}
	if isNilValue(opts.Policy.Engine) {
		return nil, fmt.Errorf("%w: nil policy engine", ErrInvalidRequest)
	}
	if !loadedConfigHashPattern.MatchString(opts.Policy.Loaded.Hash) {
		return nil, fmt.Errorf("%w: policy hash %q must be 64 lowercase hex characters",
			ErrInvalidRequest, opts.Policy.Loaded.Hash)
	}
	if len(opts.Policy.Loaded.YAML) == 0 {
		return nil, fmt.Errorf("%w: policy yaml is empty", ErrInvalidRequest)
	}

	ttl := opts.TTL
	if ttl == 0 {
		ttl = opts.Policy.Engine.ProposalTTL()
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("%w: proposal TTL must be positive, got %s", ErrInvalidRequest, ttl)
	}

	systemActor, err := audit.SystemActor("approval")
	if err != nil {
		return nil, fmt.Errorf("%w: approval actor: %v", ErrInvalidRequest, err)
	}
	expiryActor, err := audit.SystemActor("expiry")
	if err != nil {
		return nil, fmt.Errorf("%w: expiry actor: %v", ErrInvalidRequest, err)
	}
	return &Service{
		pool:        opts.Pool,
		clock:       opts.Clock,
		state:       opts.State,
		verifier:    opts.Verifier,
		limits:      opts.Limits,
		policy:      opts.Policy,
		ttl:         ttl,
		systemActor: systemActor,
		expiryActor: expiryActor,
		onClosed:    opts.OnProposalClosed,
		onInfo:      opts.InfoRequestedHook,
		tracer:      opts.Telemetry.Tracer("recon-agent/approval"),
		observer:    opts.Observer,
	}, nil
}

// isNilValue reports whether value is nil or a typed nil inside an interface.
func isNilValue(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
