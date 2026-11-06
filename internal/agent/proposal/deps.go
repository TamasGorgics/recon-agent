package proposal

import (
	"errors"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
)

// AppPool is the recon_app surface held by the deps closure: evidence reads and
// run-level trace writes. It is never placed on RunContext.
type AppPool interface {
	tools.Queryer
	tools.Execer
}

// Deps is the dependency bundle for the proposal tools.
type Deps struct {
	// App is the recon_app pool used by the default evidence reader.
	App AppPool
	// AgentExec is the recon_agent_ro write surface for the proposal INSERT.
	AgentExec tools.Execer
	// Clock supplies created_at and is required for deterministic tests.
	Clock clock.Clock
	// Ledger is the account allowlist policy (FIN-13).
	Ledger config.LedgerConfig
	// WriteOffAccounts lists account codes that satisfy the write-off rule.
	// The zero value defaults to ["suspense"].
	WriteOffAccounts []string
	// NewID generates the client-side proposal UUID. The zero value uses
	// crypto/rand UUIDv4.
	NewID func() (string, error)
	// Evidence is the authoritative evidence reader. It defaults to a DB
	// reader over App.
	Evidence EvidenceReader
}

// normalizeDeps applies defaults and validates the minimum wiring. The
// returned copy never mutates the caller's slices.
func normalizeDeps(deps Deps) (Deps, error) {
	if deps.Clock == nil {
		deps.Clock = clock.Real{}
	}
	if deps.NewID == nil {
		deps.NewID = NewUUID
	}
	if len(deps.WriteOffAccounts) == 0 {
		deps.WriteOffAccounts = []string{"suspense"}
	} else {
		deps.WriteOffAccounts = append([]string(nil), deps.WriteOffAccounts...)
	}
	deps.Ledger.AdjustmentAccounts = append([]string(nil), deps.Ledger.AdjustmentAccounts...)
	if deps.Evidence == nil {
		if deps.App == nil {
			return Deps{}, errors.New("proposal: deps need App or Evidence")
		}
		deps.Evidence = &DBEvidenceReader{Queryer: deps.App}
	}
	return deps, nil
}
