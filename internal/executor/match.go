package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/TamasGorgics/recon-agent/internal/agent/proposal"
	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
)

// Match header rules and the synthetic run configuration written by an applied
// link/duplicate proposal. They are distinct from matcher-run rules so a
// synthetic run is never mistaken for a matcher pass.
const (
	// RuleProposalLink marks an external record linked by an approved
	// reference-typo proposal.
	RuleProposalLink = "proposal_link"
	// RuleDuplicate marks an external record as a duplicate.
	RuleDuplicate = "duplicate"
	// ProposalMatchRunConfigHash is the synthetic match_runs.config_hash for
	// executor-created runs.
	ProposalMatchRunConfigHash = "executor/proposal"
	// proposalReferenceWindowDays mirrors the M4-05 verifier default
	// (config.DefaultVerifierReferenceWindowDays). The executor cannot read
	// verifier configuration from the frozen Options contract, so the
	// deterministic default is part of the apply contract.
	proposalReferenceWindowDays = 3
)

// linkPayload is the strict stored shape of a link proposal. Exactly one of
// the singular and plural ledger fields must be present.
type linkPayload struct {
	ExternalRef  *string   `json:"external_ref"`
	LedgerTxnID  *string   `json:"ledger_txn_id"`
	LedgerTxnIDs *[]string `json:"ledger_txn_ids"`
}

// duplicatePayload is the strict stored shape of a duplicate proposal.
// duplicate_of is optional and is not used for the match row; the responded
// external record is the one named by external_ref.
type duplicatePayload struct {
	ExternalRef *string `json:"external_ref"`
	DuplicateOf *string `json:"duplicate_of"`
}

// proposalScope is the apply-relevant projection of a reconciliation scope.
type proposalScope struct {
	ID              string
	LedgerAccountID string
	Source          string
	Currency        string
}

// externalRecord is the apply-relevant projection of an external record.
type externalRecord struct {
	ID             string
	Source         string
	SourceRecordID string
	AmountMinor    int64
	Currency       string
	ValueDate      time.Time
}

// proposalLeg is one match leg written for a link apply.
type proposalLeg struct {
	LedgerTxnID string
	AmountMinor int64
	Currency    string
}

// decodeLinkPayload strictly decodes payload.external_ref and exactly one of
// ledger_txn_id/ledger_txn_ids. IDs must be canonical lowercase UUIDs; the
// resulting set is sorted and unique. The external ref must be canonical.
func decodeLinkPayload(raw []byte) (string, []string, error) {
	var payload linkPayload
	if err := decodeStrictPayload(raw, &payload); err != nil {
		return "", nil, fmt.Errorf("link payload: %w", err)
	}
	if payload.ExternalRef == nil {
		return "", nil, errors.New("link payload must have external_ref")
	}
	externalRef := *payload.ExternalRef
	if err := validateExternalRef(externalRef); err != nil {
		return "", nil, err
	}
	if (payload.LedgerTxnID == nil) == (payload.LedgerTxnIDs == nil) {
		return "", nil, errors.New("link payload must have exactly one of ledger_txn_id or ledger_txn_ids")
	}
	var ids []string
	if payload.LedgerTxnID != nil {
		ids = []string{*payload.LedgerTxnID}
	} else {
		ids = *payload.LedgerTxnIDs
	}
	normalized, err := canonicalUUIDSet(ids)
	if err != nil {
		return "", nil, err
	}
	return externalRef, normalized, nil
}

// decodeDuplicatePayload strictly decodes payload.external_ref and the
// optional duplicate_of. Both, when present, must be canonical external refs.
func decodeDuplicatePayload(raw []byte) (string, string, error) {
	var payload duplicatePayload
	if err := decodeStrictPayload(raw, &payload); err != nil {
		return "", "", fmt.Errorf("duplicate payload: %w", err)
	}
	if payload.ExternalRef == nil {
		return "", "", errors.New("duplicate payload must have external_ref")
	}
	externalRef := *payload.ExternalRef
	if err := validateExternalRef(externalRef); err != nil {
		return "", "", err
	}
	if payload.DuplicateOf == nil {
		return externalRef, "", nil
	}
	duplicateOf := *payload.DuplicateOf
	if err := validateExternalRef(duplicateOf); err != nil {
		return "", "", err
	}
	return externalRef, duplicateOf, nil
}

// decodeStrictPayload decodes one JSON object with unknown fields and trailing
// data rejected. Payload columns are written only by the proposal tools, so
// any drift is a deterministic contract violation.
func decodeStrictPayload(raw []byte, dest any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return errors.New("payload is empty")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dest); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("payload has trailing data")
	}
	return nil
}

// validateExternalRef requires the canonical ext:<source>:<record id> form.
func validateExternalRef(ref string) error {
	if err := tools.ValidateRef(ref); err != nil {
		return fmt.Errorf("external ref %q is not canonical: %w", ref, err)
	}
	if !strings.HasPrefix(ref, "ext:") {
		return fmt.Errorf("external ref %q is not an ext: reference", ref)
	}
	return nil
}

// canonicalUUIDSet validates lowercase canonical UUIDs and returns a sorted
// unique set. An empty set or an uppercase/non-UUID member is an error.
func canonicalUUIDSet(ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, errors.New("ledger txn set is empty")
	}
	unique := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if !uuidPattern.MatchString(id) {
			return nil, fmt.Errorf("ledger txn id %q is not a canonical lowercase UUID", id)
		}
		unique[id] = struct{}{}
	}
	normalized := make([]string, 0, len(unique))
	for id := range unique {
		normalized = append(normalized, id)
	}
	sort.Strings(normalized)
	return normalized, nil
}

// applyLink writes the synthetic run, one active match header, and one active
// leg per proposed ledger transaction. It performs no ledger posting.
func (s *Service) applyLink(ctx context.Context, tx pgx.Tx, locked lockedProposal, resolved proposal.State, now time.Time) error {
	externalRef, ledgerTxnIDs, err := decodeLinkPayload(locked.Payload)
	if err != nil {
		return &revalidationFailure{reason: FailureStateChanged, cause: err}
	}
	target, err := linkBreak(resolved, externalRef)
	if err != nil {
		return &revalidationFailure{reason: FailureStateChanged, cause: err}
	}
	scope, err := loadProposalScope(ctx, tx, target.ID)
	if err != nil {
		return revalidationFailureFor(err, FailureStateChanged)
	}
	if scope.Currency != target.Currency {
		return &revalidationFailure{
			reason: FailureStateChanged,
			cause:  fmt.Errorf("break %s currency %s differs from scope %s", target.ID, target.Currency, scope.Currency),
		}
	}
	external, err := loadExternalRecord(ctx, tx, externalRef)
	if err != nil {
		return revalidationFailureFor(err, FailureStateChanged)
	}
	if external.Source != scope.Source || external.Currency != scope.Currency {
		return &revalidationFailure{
			reason: FailureStateChanged,
			cause: fmt.Errorf("external record %s/%s (%s) does not belong to scope %s/%s",
				external.Source, external.SourceRecordID, external.Currency, scope.Source, scope.Currency),
		}
	}
	if err := ensureNoActiveMatch(ctx, tx, external.ID); err != nil {
		return revalidationFailureFor(err, FailureStateChanged)
	}
	legs, err := s.linkLegs(ctx, tx, scope, target, external, ledgerTxnIDs)
	if err != nil {
		return revalidationFailureFor(err, FailureStateChanged)
	}
	return s.insertProposalMatch(ctx, tx, scope, external.ID, RuleProposalLink, legs, now)
}

// applyDuplicate writes the synthetic run and an active duplicate header with
// no legs. It performs no ledger posting.
func (s *Service) applyDuplicate(ctx context.Context, tx pgx.Tx, locked lockedProposal, resolved proposal.State, now time.Time) error {
	externalRef, _, err := decodeDuplicatePayload(locked.Payload)
	if err != nil {
		return &revalidationFailure{reason: FailureStateChanged, cause: err}
	}
	target, err := duplicateBreak(resolved, externalRef)
	if err != nil {
		return &revalidationFailure{reason: FailureStateChanged, cause: err}
	}
	scope, err := loadProposalScope(ctx, tx, target.ID)
	if err != nil {
		return revalidationFailureFor(err, FailureStateChanged)
	}
	if scope.Currency != target.Currency {
		return &revalidationFailure{
			reason: FailureStateChanged,
			cause:  fmt.Errorf("break %s currency %s differs from scope %s", target.ID, target.Currency, scope.Currency),
		}
	}
	external, err := loadExternalRecord(ctx, tx, externalRef)
	if err != nil {
		return revalidationFailureFor(err, FailureStateChanged)
	}
	if external.Source != scope.Source || external.Currency != scope.Currency {
		return &revalidationFailure{
			reason: FailureStateChanged,
			cause: fmt.Errorf("external record %s/%s (%s) does not belong to scope %s/%s",
				external.Source, external.SourceRecordID, external.Currency, scope.Source, scope.Currency),
		}
	}
	if err := ensureNoActiveMatch(ctx, tx, external.ID); err != nil {
		return revalidationFailureFor(err, FailureStateChanged)
	}
	return s.insertProposalMatch(ctx, tx, scope, external.ID, RuleDuplicate, nil, now)
}

// linkBreak finds the single break carrying externalRef and enforces the link
// payload binding: a link break has exactly one external reference and that
// reference is the payload's.
func linkBreak(resolved proposal.State, externalRef string) (proposal.BreakState, error) {
	var matched []proposal.BreakState
	for _, item := range resolved.Breaks {
		for _, ref := range item.ExternalRefs {
			if ref == externalRef {
				matched = append(matched, item)
				break
			}
		}
	}
	if len(matched) != 1 {
		return proposal.BreakState{}, fmt.Errorf("external ref %s belongs to %d attached breaks, want 1", externalRef, len(matched))
	}
	target := matched[0]
	if len(target.ExternalRefs) != 1 {
		return proposal.BreakState{}, fmt.Errorf("link break %s has %d external refs, want 1", target.ID, len(target.ExternalRefs))
	}
	return target, nil
}

// duplicateBreak finds the single attached break whose external refs include
// the payload's external reference. A duplicate break may carry several
// external records; the marked one only has to be present.
func duplicateBreak(resolved proposal.State, externalRef string) (proposal.BreakState, error) {
	var matched []proposal.BreakState
	for _, item := range resolved.Breaks {
		for _, ref := range item.ExternalRefs {
			if ref == externalRef {
				matched = append(matched, item)
				break
			}
		}
	}
	if len(matched) != 1 {
		return proposal.BreakState{}, fmt.Errorf("external ref %s belongs to %d attached breaks, want 1", externalRef, len(matched))
	}
	return matched[0], nil
}

// loadProposalScope resolves the reconciliation scope behind a break.
func loadProposalScope(ctx context.Context, tx pgx.Tx, breakID string) (proposalScope, error) {
	var scope proposalScope
	err := tx.QueryRow(ctx,
		`SELECT s.id::text, s.ledger_account_id::text, s.source, s.currency
		   FROM breaks b
		   JOIN reconciliation_scopes s ON s.id = b.scope_id
		  WHERE b.id = $1::uuid`, breakID).
		Scan(&scope.ID, &scope.LedgerAccountID, &scope.Source, &scope.Currency)
	if errors.Is(err, pgx.ErrNoRows) {
		return proposalScope{}, fmt.Errorf("break %s has no reconciliation scope", breakID)
	}
	if err != nil {
		return proposalScope{}, fmt.Errorf("load break scope: %w", err)
	}
	return scope, nil
}

// loadExternalRecord resolves a canonical external ref to its stored record.
func loadExternalRecord(ctx context.Context, tx pgx.Tx, externalRef string) (externalRecord, error) {
	source, recordID, err := splitExternalRef(externalRef)
	if err != nil {
		return externalRecord{}, err
	}
	var external externalRecord
	err = tx.QueryRow(ctx,
		`SELECT id::text, source, source_record_id, amount_minor, currency, value_date
		   FROM external_records
		  WHERE source = $1 AND source_record_id = $2`,
		source, recordID).
		Scan(&external.ID, &external.Source, &external.SourceRecordID,
			&external.AmountMinor, &external.Currency, &external.ValueDate)
	if errors.Is(err, pgx.ErrNoRows) {
		return externalRecord{}, fmt.Errorf("external record %s does not exist", externalRef)
	}
	if err != nil {
		return externalRecord{}, fmt.Errorf("load external record: %w", err)
	}
	return external, nil
}

// ensureNoActiveMatch rejects an external record already claimed by an active
// match header. The partial unique index is the concurrency backstop.
func ensureNoActiveMatch(ctx context.Context, tx pgx.Tx, externalRecordID string) error {
	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (
		     SELECT 1 FROM matches
		      WHERE external_record_id = $1::uuid AND status = 'active')`,
		externalRecordID).Scan(&exists); err != nil {
		return fmt.Errorf("check active match: %w", err)
	}
	if exists {
		return fmt.Errorf("external record %s already has an active match", externalRecordID)
	}
	return nil
}

// linkLegs enforces the break/payload ledger-set binding and computes one
// signed net leg per proposed ledger transaction on the scope's ledger
// account. A break that names ledger transactions requires exactly that set;
// an external-only break requires the unique M4-05 scope candidate.
func (s *Service) linkLegs(ctx context.Context, tx pgx.Tx, scope proposalScope, target proposal.BreakState, external externalRecord, proposed []string) ([]proposalLeg, error) {
	if len(target.LedgerTxnIDs) > 0 {
		if !sameSet(target.LedgerTxnIDs, proposed) {
			return nil, fmt.Errorf("proposed ledger set does not match break %s ledger set", target.ID)
		}
	} else {
		candidates, err := loadReferenceCandidates(ctx, tx, scope, external)
		if err != nil {
			return nil, err
		}
		switch len(candidates) {
		case 0:
			return nil, fmt.Errorf("external record %s has no eligible ledger candidate", external.ID)
		case 1:
			if !sameSet(candidates, proposed) {
				return nil, fmt.Errorf("proposed ledger set is not the unique scope candidate %s", candidates[0])
			}
		default:
			return nil, fmt.Errorf("external record %s has %d eligible ledger candidates, want 1", external.ID, len(candidates))
		}
	}
	return loadSignedLegs(ctx, tx, scope, proposed)
}

// loadReferenceCandidates mirrors the M4-05 reference_typo candidate rule:
// same scope account and currency, signed amount equal to the external amount,
// value date inside the inclusive window, and no active match leg. Ordering is
// by ledger transaction ID so ambiguity is deterministic.
func loadReferenceCandidates(ctx context.Context, tx pgx.Tx, scope proposalScope, external externalRecord) ([]string, error) {
	rows, err := tx.Query(ctx,
		`SELECT t.id::text
		   FROM ledger_txns t
		   JOIN ledger_entries e ON e.txn_id = t.id
		  WHERE e.account_id = $1::uuid
		    AND e.currency = $3
		    AND t.value_date BETWEEN $4::date - $5::int AND $4::date + $5::int
		    AND NOT EXISTS (
		        SELECT 1 FROM match_legs ml
		         WHERE ml.ledger_txn_id = t.id AND ml.status = 'active')
		  GROUP BY t.id
		 HAVING SUM(CASE WHEN e.direction = 'debit'
		                 THEN e.amount_minor ELSE -e.amount_minor END)::bigint = $2
		  ORDER BY t.id`,
		scope.LedgerAccountID, external.AmountMinor, scope.Currency,
		external.ValueDate.UTC().Format("2006-01-02"), proposalReferenceWindowDays)
	if err != nil {
		return nil, fmt.Errorf("load reference candidates: %w", err)
	}
	defer rows.Close()

	var candidates []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan reference candidate: %w", err)
		}
		candidates = append(candidates, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate reference candidates: %w", err)
	}
	return candidates, nil
}

// loadSignedLegs computes the signed net (debit positive, credit negative) on
// the scope ledger account for every proposed transaction. A missing entry or
// a mismatched currency is a deterministic state_changed failure.
func loadSignedLegs(ctx context.Context, tx pgx.Tx, scope proposalScope, ledgerTxnIDs []string) ([]proposalLeg, error) {
	if err := ensureNoActiveLegs(ctx, tx, ledgerTxnIDs); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx,
		`SELECT txn_id::text,
		        SUM(CASE WHEN direction = 'debit' THEN amount_minor ELSE -amount_minor END)::bigint,
		        COUNT(*) FILTER (WHERE currency <> $2)::int,
		        COUNT(*)::int
		   FROM ledger_entries
		  WHERE txn_id = ANY($1::uuid[]) AND account_id = $3::uuid
		  GROUP BY txn_id
		  ORDER BY txn_id`,
		ledgerTxnIDs, scope.Currency, scope.LedgerAccountID)
	if err != nil {
		return nil, fmt.Errorf("load scope ledger entries: %w", err)
	}
	defer rows.Close()

	type legRow struct {
		amount     int64
		mismatched int
		entries    int
	}
	found := make(map[string]legRow, len(ledgerTxnIDs))
	for rows.Next() {
		var (
			id  string
			row legRow
		)
		if err := rows.Scan(&id, &row.amount, &row.mismatched, &row.entries); err != nil {
			return nil, fmt.Errorf("scan scope ledger entry: %w", err)
		}
		found[id] = row
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate scope ledger entries: %w", err)
	}

	legs := make([]proposalLeg, 0, len(ledgerTxnIDs))
	for _, id := range ledgerTxnIDs {
		row, ok := found[id]
		if !ok || row.entries == 0 {
			return nil, fmt.Errorf("ledger transaction %s has no entry on scope account %s", id, scope.LedgerAccountID)
		}
		if row.mismatched > 0 {
			return nil, fmt.Errorf("ledger transaction %s has %d entries outside scope currency %s", id, row.mismatched, scope.Currency)
		}
		legs = append(legs, proposalLeg{
			LedgerTxnID: id,
			AmountMinor: row.amount,
			Currency:    scope.Currency,
		})
	}
	return legs, nil
}

// ensureNoActiveLegs rejects any ledger transaction already claimed by an
// active match leg. The partial unique index is the concurrency backstop.
func ensureNoActiveLegs(ctx context.Context, tx pgx.Tx, ledgerTxnIDs []string) error {
	var claimed []string
	rows, err := tx.Query(ctx,
		`SELECT DISTINCT ledger_txn_id::text
		   FROM match_legs
		  WHERE ledger_txn_id = ANY($1::uuid[]) AND status = 'active'
		  ORDER BY ledger_txn_id`,
		ledgerTxnIDs)
	if err != nil {
		return fmt.Errorf("check active match legs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scan active match leg: %w", err)
		}
		claimed = append(claimed, id)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate active match legs: %w", err)
	}
	if len(claimed) > 0 {
		return fmt.Errorf("ledger transactions already have active match legs: %s", strings.Join(claimed, ", "))
	}
	return nil
}

// insertProposalMatch creates the synthetic succeeded run, the active match
// header, and (for a link) its legs. A unique violation means a concurrent
// matcher or executor claimed the external record or a ledger transaction; it
// is a deterministic state_changed failure, never an infrastructure retry.
func (s *Service) insertProposalMatch(ctx context.Context, tx pgx.Tx, scope proposalScope, externalRecordID, rule string, legs []proposalLeg, now time.Time) error {
	var runID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO match_runs
		     (scope_id, started_at, finished_at, status, stats, config_hash)
		 VALUES ($1::uuid, $2, $2, 'succeeded', '{}'::jsonb, $3)
		 RETURNING id::text`,
		scope.ID, now, ProposalMatchRunConfigHash).Scan(&runID); err != nil {
		return fmt.Errorf("insert synthetic match run: %w", err)
	}

	var matchID string
	err := tx.QueryRow(ctx,
		`INSERT INTO matches
		     (scope_id, external_record_id, rule, confidence, match_run_id,
		      status, created_at)
		 VALUES ($1::uuid, $2::uuid, $3, 1.0000, $4::uuid, 'active', $5)
		 RETURNING id::text`,
		scope.ID, externalRecordID, rule, runID, now).Scan(&matchID)
	if err != nil {
		if isUniqueViolation(err) {
			return &revalidationFailure{
				reason: FailureStateChanged,
				cause:  fmt.Errorf("external record %s gained an active match concurrently", externalRecordID),
			}
		}
		return fmt.Errorf("insert proposal match header: %w", err)
	}

	for _, leg := range legs {
		if _, err := tx.Exec(ctx,
			`INSERT INTO match_legs
			     (match_id, ledger_txn_id, amount_minor, currency, status, created_at)
			 VALUES ($1::uuid, $2::uuid, $3, $4, 'active', $5)`,
			matchID, leg.LedgerTxnID, leg.AmountMinor, leg.Currency, now); err != nil {
			if isUniqueViolation(err) {
				return &revalidationFailure{
					reason: FailureStateChanged,
					cause:  fmt.Errorf("ledger transaction %s gained an active match leg concurrently", leg.LedgerTxnID),
				}
			}
			return fmt.Errorf("insert proposal match leg %s: %w", leg.LedgerTxnID, err)
		}
	}
	return nil
}

// sameSet reports whether two UUID lists name the same non-empty set.
func sameSet(left, right []string) bool {
	if len(left) == 0 || len(left) != len(right) {
		return false
	}
	leftSet := make(map[string]struct{}, len(left))
	for _, id := range left {
		leftSet[id] = struct{}{}
	}
	if len(leftSet) != len(left) {
		return false
	}
	for _, id := range right {
		if _, ok := leftSet[id]; !ok {
			return false
		}
	}
	return true
}

// isUniqueViolation reports a PostgreSQL 23505 constraint conflict.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
