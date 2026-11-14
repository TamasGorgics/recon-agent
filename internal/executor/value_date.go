package executor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/agent/proposal"
)

// valueDate chooses the adjustment/write-off value date:
//  1. the earliest external_records.value_date referenced by the case breaks;
//  2. otherwise the earliest ledger_txns.value_date among the proposal's cited
//     ledger_txn: evidence refs;
//  3. otherwise the clock's UTC date.
//
// The break's ledger_txn_ids are deliberately not used: they describe the
// matched ledger side, not the evidence the proposal cited. The adjustment
// therefore stays in the break period; ledger.Post owns the closed-period
// redirect (FIN-56).
//
// A malformed cited ledger reference is a deterministic error; database errors
// are returned as-is so the caller's revalidationFailureFor keeps them
// infrastructure and leaves the proposal approved.
func (s *Service) valueDate(ctx context.Context, tx pgx.Tx, resolved proposal.State, rawEvidence []byte) (time.Time, error) {
	evidence, err := decodeEvidence(rawEvidence)
	if err != nil {
		return time.Time{}, fmt.Errorf("executor: value date evidence: %w", err)
	}
	ledgerTxnIDs, err := citedLedgerTxnIDs(evidence)
	if err != nil {
		return time.Time{}, err
	}

	var (
		sources   []string
		recordIDs []string
	)
	for _, item := range resolved.Breaks {
		for _, ref := range item.ExternalRefs {
			source, recordID, err := splitExternalRef(ref)
			if err != nil {
				return time.Time{}, fmt.Errorf("executor: break %s external ref: %w", item.ID, err)
			}
			sources = append(sources, source)
			recordIDs = append(recordIDs, recordID)
		}
	}

	if len(sources) > 0 {
		var valueDate *time.Time
		if err := tx.QueryRow(ctx,
			`SELECT min(value_date)
			   FROM external_records
			  WHERE (source, source_record_id) IN (
			        SELECT * FROM unnest($1::text[], $2::text[]))`,
			sources, recordIDs).Scan(&valueDate); err != nil {
			return time.Time{}, fmt.Errorf("executor: load break external value date: %w", err)
		}
		if valueDate != nil {
			return dateOnly(*valueDate), nil
		}
	}

	if len(ledgerTxnIDs) > 0 {
		var valueDate *time.Time
		if err := tx.QueryRow(ctx,
			`SELECT min(value_date) FROM ledger_txns WHERE id = ANY($1::uuid[])`,
			ledgerTxnIDs).Scan(&valueDate); err != nil {
			return time.Time{}, fmt.Errorf("executor: load cited ledger value date: %w", err)
		}
		if valueDate != nil {
			return dateOnly(*valueDate), nil
		}
	}

	return dateOnly(s.clock.Now().UTC()), nil
}

// citedLedgerTxnIDs extracts the canonical ledger_txn:<uuid> evidence refs,
// deduplicated. Non-ledger evidence is ignored; a malformed ledger ref is a
// deterministic error.
func citedLedgerTxnIDs(evidence []string) ([]string, error) {
	ids := make([]string, 0, len(evidence))
	seen := make(map[string]struct{}, len(evidence))
	for _, ref := range evidence {
		id, ok := strings.CutPrefix(ref, "ledger_txn:")
		if !ok {
			continue
		}
		if !uuidPattern.MatchString(id) {
			return nil, fmt.Errorf("executor: cited ledger ref %q is not a canonical lowercase UUID", ref)
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids, nil
}

// splitExternalRef splits the canonical ext:<source>:<record id> form. The
// record id may itself contain colons, so only the source prefix is cut.
func splitExternalRef(ref string) (string, string, error) {
	rest, ok := strings.CutPrefix(ref, "ext:")
	if !ok {
		return "", "", fmt.Errorf("%q is not an ext: reference", ref)
	}
	source, recordID, ok := strings.Cut(rest, ":")
	if !ok || source == "" || recordID == "" {
		return "", "", fmt.Errorf("%q is not a canonical ext: reference", ref)
	}
	return source, recordID, nil
}

// dateOnly normalizes a timestamp or date to midnight UTC on its business
// date.
func dateOnly(value time.Time) time.Time {
	utc := value.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
}
