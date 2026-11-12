package policy

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

// Persistence errors. Both are returned before raw SQLSTATEs reach callers.
var (
	// ErrInvalidProposalID reports a proposal ID that is not a canonical
	// lowercase UUID.
	ErrInvalidProposalID = errors.New("policy: invalid proposal id")
	// ErrProposalNotFound reports that PersistDecision matched no proposal row.
	ErrProposalNotFound = errors.New("policy: proposal not found")
	// ErrUnknownConfig reports that Decision.ConfigHash has no
	// policy_config_versions row. M4-11 must EnsureVersion before persisting a
	// decision so every decision remains explainable against its exact document.
	ErrUnknownConfig = errors.New("policy: unknown config hash")
	// ErrInvalidVersionInput reports a missing transaction or clock value.
	ErrInvalidVersionInput = errors.New("policy: invalid version input")
)

var canonicalUUIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// EnsureVersion content-addresses the loaded policy document. The first call
// for a hash inserts one row; later calls are no-ops. If the hash already
// exists with different YAML bytes, the call fails closed with
// ErrConfigHashMismatch and never overwrites the stored row. It uses the
// caller's transaction and never starts, commits, or rolls back one.
func (l LoadedConfig) EnsureVersion(ctx context.Context, tx pgx.Tx, loadedAt time.Time) error {
	if tx == nil {
		return fmt.Errorf("%w: nil transaction", ErrInvalidVersionInput)
	}
	if loadedAt.IsZero() {
		return fmt.Errorf("%w: zero loaded_at", ErrInvalidVersionInput)
	}
	if !configHashPattern.MatchString(l.Hash) {
		return fmt.Errorf("%w: hash %q must be 64 lowercase hex characters", ErrInvalidConfig, l.Hash)
	}
	if got := HashYAML(l.YAML); got != l.Hash {
		return fmt.Errorf("%w: hash %q does not match sha256(%s)", ErrInvalidConfig, l.Hash, got)
	}
	if err := l.Config.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}

	loadedAt = loadedAt.UTC().Truncate(time.Microsecond)
	tag, err := tx.Exec(ctx,
		`INSERT INTO policy_config_versions (hash, yaml, loaded_at)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (hash) DO NOTHING`,
		l.Hash, string(l.YAML), loadedAt)
	if err != nil {
		return fmt.Errorf("policy: insert config version: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}

	var stored string
	err = tx.QueryRow(ctx,
		`SELECT yaml FROM policy_config_versions WHERE hash = $1`, l.Hash).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrConfigVersionMissing, l.Hash)
	}
	if err != nil {
		return fmt.Errorf("policy: read config version: %w", err)
	}
	if stored != string(l.YAML) {
		return fmt.Errorf("%w: stored YAML for %s differs from loaded bytes", ErrConfigHashMismatch, l.Hash)
	}
	return nil
}

// PersistDecision writes the policy decision into proposals.risk_tier and
// proposals.policy_decision only. The decision's ConfigHash must already have a
// policy_config_versions row (EnsureVersion); otherwise it fails closed with
// ErrUnknownConfig before touching the proposal. It uses the caller's
// transaction, writes no audit row, and never touches status, version,
// expires_at, payload, rationale, evidence, state_hash, or verification. M4-11
// calls EnsureVersion and PersistDecision in the same transaction as the state
// transition.
func PersistDecision(ctx context.Context, tx pgx.Tx, proposalID string, d Decision) error {
	if tx == nil {
		return fmt.Errorf("%w: nil transaction", ErrInvalidVersionInput)
	}
	if !canonicalUUIDPattern.MatchString(proposalID) {
		return fmt.Errorf("%w: %q is not a canonical lowercase UUID", ErrInvalidProposalID, proposalID)
	}
	payload, err := d.JSON()
	if err != nil {
		return err
	}

	var known bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM policy_config_versions WHERE hash = $1)`,
		d.ConfigHash).Scan(&known); err != nil {
		return fmt.Errorf("policy: look up config version: %w", err)
	}
	if !known {
		return fmt.Errorf("%w: %s", ErrUnknownConfig, d.ConfigHash)
	}

	tag, err := tx.Exec(ctx,
		`UPDATE proposals
		    SET risk_tier = $2, policy_decision = $3
		  WHERE id = $1::uuid`,
		proposalID, string(d.Tier), string(payload))
	if err != nil {
		return fmt.Errorf("policy: persist decision: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s", ErrProposalNotFound, proposalID)
	}
	return nil
}
