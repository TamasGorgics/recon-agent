package lifecycle

import (
	"fmt"
	"reflect"

	"github.com/TamasGorgics/recon-agent/internal/approval/state"
	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/bizdays"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/db"
)

// The lifecycle service's fixed identifiers. The proposal supersede edges are
// system-actor edges in the M4-03 authority; a human unmatch/late-match caller
// is recorded on the case.proposal_superseded audit, while the proposal's own
// transition audit names the system component that performed the write.
const (
	// ProposalActorID is the system actor used for internal proposal
	// transitions. It keeps agent and human actors out of system-only
	// supersede edges.
	ProposalActorID = "case-lifecycle"
)

// Options is the lifecycle service dependency set. DB and Clock are required.
// DB is the application-role handle (recon_app in production); later sweeps
// open their own transactions through it, and the proposal authority uses it
// for its own state service.
type Options struct {
	// DB starts later-wave sweep transactions and backs the proposal state
	// service. *pgxpool.Pool satisfies it.
	DB db.Beginner
	// Clock supplies every audit timestamp; the service never reads the wall
	// clock.
	Clock clock.Clock
	// Sources is the per-source FIN-41 calendar set used by the
	// missing-settlement SLA sweep. It is optional for the other lifecycle
	// operations; an SLA sweep with no calendars configured evaluates nothing.
	Sources bizdays.Sources
	// MissingSettlementSLABusinessDays is the configured
	// cases.missing_settlement_sla_business_days value. Zero selects the
	// documented default (5). Any other value outside 1..365 is rejected at
	// construction; the config layer rejects it at startup too.
	MissingSettlementSLABusinessDays int
}

// Service owns the M4-10 lifecycle operations that have been implemented.
// SupersedeActiveProposal only uses the caller's transaction, so the service
// holds no pool of its own for that method.
type Service struct {
	database        db.Beginner
	clock           clock.Clock
	proposals       *state.Service
	proposalActor   audit.Actor
	actor           audit.Actor
	sources         bizdays.Sources
	slaBusinessDays int
}

// NewService validates its dependencies and returns a lifecycle service.
// A nil database or clock is a construction error wrapping ErrInvalidRequest.
func NewService(opts Options) (*Service, error) {
	if isNilValue(opts.DB) {
		return nil, fmt.Errorf("%w: nil database", ErrInvalidRequest)
	}
	if isNilValue(opts.Clock) {
		return nil, fmt.Errorf("%w: nil clock", ErrInvalidRequest)
	}
	proposals, err := state.NewService(opts.DB, opts.Clock)
	if err != nil {
		return nil, fmt.Errorf("%w: proposal state service: %v", ErrInvalidRequest, err)
	}
	proposalActor, err := audit.SystemActor(ProposalActorID)
	if err != nil {
		return nil, fmt.Errorf("%w: proposal actor: %v", ErrInvalidRequest, err)
	}
	lifecycleActor, err := audit.SystemActor(caseLifecycleActorID)
	if err != nil {
		return nil, fmt.Errorf("%w: lifecycle actor: %v", ErrInvalidRequest, err)
	}
	slaDays := opts.MissingSettlementSLABusinessDays
	if slaDays == 0 {
		slaDays = config.DefaultMissingSettlementSLABusinessDays
	}
	if slaDays < 1 || slaDays > 365 {
		return nil, fmt.Errorf("%w: %s must be between 1 and 365, got %d",
			ErrInvalidRequest, config.MissingSettlementSLAKey, slaDays)
	}
	return &Service{
		database:        opts.DB,
		clock:           opts.Clock,
		proposals:       proposals,
		proposalActor:   proposalActor,
		actor:           lifecycleActor,
		sources:         opts.Sources,
		slaBusinessDays: slaDays,
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
