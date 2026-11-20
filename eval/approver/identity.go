package approver

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// The fixed eval-only identity. The UUID is a canonical lowercase v4 UUID and
// is never an agent actor, a proposal author, or a case co-maker, so the real
// approval service's four-eyes check passes on the normal path.
const (
	// SyntheticApproverID is the seeded simulated-approver UUID.
	SyntheticApproverID = "00000000-0000-4000-8000-00000000e2e1"
	// SyntheticApproverDisplayName is the seeded human-readable name.
	SyntheticApproverDisplayName = "Eval simulated approver"
	// SyntheticApproverRole is the seeded approvers.role value.
	SyntheticApproverRole = "approver"
	// SyntheticSource is the approval source recorded for every simulated
	// decision. It distinguishes a simulated decision from a human one.
	SyntheticSource = "e2e"
)

// Execer is the narrow write surface EnsureSyntheticApprover needs. The eval
// sandbox admin pool satisfies it; the recon_app role intentionally has no
// INSERT grant on approvers.
type Execer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// EnsureSyntheticApprover seeds the synthetic approver row in an eval sandbox.
// It is idempotent: a second call for the same id writes nothing. The caller
// must pass a sandbox/admin database connection; this package never seeds a
// production database.
func EnsureSyntheticApprover(ctx context.Context, execer Execer, id string) error {
	if isNilValue(execer) {
		return fmt.Errorf("eval/approver: ensure synthetic approver: nil execer")
	}
	if err := validateApproverID(id); err != nil {
		return err
	}
	if _, err := execer.Exec(ctx,
		`INSERT INTO approvers
		     (id, display_name, role, active, slack_user_id, ticket_user_id)
		 VALUES ($1::uuid, $2, $3, true, NULL, NULL)
		 ON CONFLICT (id) DO NOTHING`,
		id, SyntheticApproverDisplayName, SyntheticApproverRole); err != nil {
		return fmt.Errorf("eval/approver: seed synthetic approver: %w", err)
	}
	return nil
}

// validateApproverID rejects agent/system actors (FIN-33) and requires the
// canonical lowercase UUID form the approval service and approvers table use.
func validateApproverID(id string) error {
	switch {
	case id == "":
		return fmt.Errorf("%w: approver id is required", ErrNoApprover)
	case id != strings.TrimSpace(id):
		return fmt.Errorf("%w: approver id is padded", ErrNoApprover)
	case id == "agent" || strings.HasPrefix(id, "agent:"),
		id == "system" || strings.HasPrefix(id, "system:"):
		return fmt.Errorf("%w: agent and system actors cannot decide", ErrNoApprover)
	case !canonicalUUIDPattern.MatchString(id):
		return fmt.Errorf("%w: approver id is not a canonical lowercase UUID", ErrNoApprover)
	default:
		return nil
	}
}
