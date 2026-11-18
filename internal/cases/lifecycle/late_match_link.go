package lifecycle

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Match statuses from the M1-12 schema. A late match reconciles only an active
// match; an unmatched row is a historical link and is deliberately a no-op.
const (
	matchStatusActive    = "active"
	matchStatusUnmatched = "unmatched"
)

// matchLink is the immutable identifier projection of one match. There is no
// breaks.match_id column, so a break is linked to a match through the two
// identifier sets the matcher already writes:
//
//   - match_legs.ledger_txn_id, compared with breaks.ledger_txn_ids; or
//   - the match's external record (external_records.source,
//     external_records.source_record_id), compared with breaks.external_refs,
//     which is a JSON array of {"source","source_record_id"} objects.
//
// Both comparisons are scoped to the match's reconciliation scope. The
// external-ref comparison is JSONB containment: a break carries the match's
// external ref when external_refs @> [{"source":…,"source_record_id":…}].
type matchLink struct {
	MatchID          string
	ScopeID          string
	Status           string
	ExternalRecordID string
	Source           string
	SourceRecordID   string
	LedgerTxnIDs     []string
}

// active reports whether the match row is still active.
func (l matchLink) active() bool {
	return l.Status == matchStatusActive
}

// loadActiveMatchLink locks the match header and loads its identifier
// projection. A missing row is ErrMatchNotFound. The match row is acquired
// first, before any case, proposal, or break lock, to honor the global lock
// order.
func loadActiveMatchLink(ctx context.Context, tx pgx.Tx, matchID string) (matchLink, error) {
	link := matchLink{MatchID: matchID}
	err := tx.QueryRow(ctx,
		`SELECT m.scope_id::text, m.status, m.external_record_id::text,
		        er.source, er.source_record_id
		   FROM matches m
		   JOIN external_records er ON er.id = m.external_record_id
		  WHERE m.id = $1::uuid
		    FOR UPDATE OF m`, matchID).
		Scan(&link.ScopeID, &link.Status, &link.ExternalRecordID,
			&link.Source, &link.SourceRecordID)
	if errors.Is(err, pgx.ErrNoRows) {
		return matchLink{}, fmt.Errorf("%w: %s", ErrMatchNotFound, matchID)
	}
	if err != nil {
		return matchLink{}, fmt.Errorf("cases/lifecycle: lock match %s: %w", matchID, err)
	}
	if link.Status != matchStatusActive && link.Status != matchStatusUnmatched {
		return matchLink{}, fmt.Errorf("%w: stored %q", ErrInvalidMatchStatus, link.Status)
	}
	if !link.active() {
		return link, nil
	}

	rows, err := tx.Query(ctx,
		`SELECT ledger_txn_id::text
		   FROM match_legs
		  WHERE match_id = $1::uuid AND status = 'active'
		  ORDER BY ledger_txn_id`, matchID)
	if err != nil {
		return matchLink{}, fmt.Errorf("cases/lifecycle: load active legs for match %s: %w",
			matchID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var ledgerTxnID string
		if err := rows.Scan(&ledgerTxnID); err != nil {
			return matchLink{}, fmt.Errorf("cases/lifecycle: scan match leg: %w", err)
		}
		link.LedgerTxnIDs = append(link.LedgerTxnIDs, ledgerTxnID)
	}
	if err := rows.Err(); err != nil {
		return matchLink{}, fmt.Errorf("cases/lifecycle: iterate match legs: %w", err)
	}
	return link, nil
}

// consumedBreakIDs returns the ids of breaks in the match's scope that the
// match consumed, ordered by id. It is the single linkage authority used by
// ReconcileLateMatch; both branches are exercised by the linkage integration
// test.
func consumedBreakIDs(ctx context.Context, tx pgx.Tx, link matchLink) ([]string, error) {
	if !link.active() {
		return nil, nil
	}
	rows, err := tx.Query(ctx,
		`SELECT b.id::text
		   FROM breaks b
		  WHERE b.scope_id = $1::uuid
		    AND (
		         b.ledger_txn_ids && $2::text[]::uuid[]
		         OR b.external_refs @> jsonb_build_array(
		                jsonb_build_object('source', $3::text,
		                                   'source_record_id', $4::text))
		        )
		  ORDER BY b.id`,
		link.ScopeID, link.LedgerTxnIDs, link.Source, link.SourceRecordID)
	if err != nil {
		return nil, fmt.Errorf("cases/lifecycle: load consumed breaks for match %s: %w",
			link.MatchID, err)
	}
	defer rows.Close()

	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("cases/lifecycle: scan consumed break: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cases/lifecycle: iterate consumed breaks: %w", err)
	}
	return ids, nil
}

// caseIDsForBreaks returns the distinct cases attached to breakIDs in
// ascending case-id order, the order in which ReconcileLateMatch locks them.
func caseIDsForBreaks(ctx context.Context, tx pgx.Tx, breakIDs []string) ([]string, error) {
	if len(breakIDs) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx,
		`SELECT DISTINCT cb.case_id::text AS case_id
		   FROM case_breaks cb
		  WHERE cb.break_id::text = ANY($1::text[])
		  ORDER BY case_id`, breakIDs)
	if err != nil {
		return nil, fmt.Errorf("cases/lifecycle: load cases for consumed breaks: %w", err)
	}
	defer rows.Close()

	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("cases/lifecycle: scan case id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cases/lifecycle: iterate case ids: %w", err)
	}
	return ids, nil
}
