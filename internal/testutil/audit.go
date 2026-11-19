// Package testutil provides helpers shared by integration tests across
// packages. It must never be imported by production code.
package testutil

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/db"
)

// Queryer is the subset of pgx used by audit assertions. It is satisfied by
// *pgxpool.Pool and pgx.Tx, so callers can assert inside or after a transaction.
// The completeness helpers only issue SELECTs through Query.
type Queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Expectation is one exact (Action, EntityType, EntityID) audit triple. It is
// the unit of the completeness assertions: every field must match, and
// duplicates count once.
type Expectation struct {
	Action     string
	EntityType string
	EntityID   string
}

// String renders the triple for failure messages. Values are quoted so
// whitespace and empty fields remain visible.
func (e Expectation) String() string {
	return fmt.Sprintf("action=%q entity_type=%q entity_id=%q", e.Action, e.EntityType, e.EntityID)
}

// reporter is the subset of *testing.T the completeness checker needs. Keeping
// it an interface lets unit tests prove the all-in-one-failure behavior without
// a subprocess.
type reporter interface {
	Helper()
	Errorf(format string, args ...any)
}

// AssertAudited fails the test when no audit row names entityID and returns the
// number of audit rows for it. It lets every Post/Reverse test prove the audit
// entry committed in the same transaction (FIN-30).
func AssertAudited(t *testing.T, q Queryer, entityID string) int64 {
	t.Helper()
	var count int64
	if err := q.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_log WHERE entity_id = $1`, entityID).Scan(&count); err != nil {
		t.Fatalf("testutil: count audit rows for %q: %v", entityID, err)
	}
	if count == 0 {
		t.Fatalf("testutil: no audit row for entity %q", entityID)
	}
	return count
}

// AppendAudit appends e on b in its own committed transaction and fails the
// test on error. Use it to seed audit rows; use audit.Append directly when the
// test needs to observe rollback or chain mechanics.
func AppendAudit(t *testing.T, ctx context.Context, b db.Beginner, e audit.Entry) audit.Record {
	t.Helper()
	var record audit.Record
	err := db.WithTx(ctx, b, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		record, err = audit.Append(ctx, tx, e)
		return err
	})
	if err != nil {
		t.Fatalf("testutil: append audit entry: %v", err)
	}
	return record
}

// AssertAuditCompleteness fails when any expected audit triple is missing from
// audit_log. Matching is exact on all three fields; duplicate expectations and
// duplicate audit rows each count once. All missing triples are collected and
// reported in one failure, sorted by entity type, entity ID, then action.
//
// The check reads through q (a pool or the caller's transaction) and performs
// no writes, so a transaction sees its own uncommitted audit rows.
func AssertAuditCompleteness(t *testing.T, q Queryer, expected []Expectation) {
	t.Helper()
	checkAuditCompleteness(t, expected, loadAuditTriples(t, q), nil, false)
}

// AssertAuditCompletenessExact additionally rejects audit triples that are
// present in audit_log but absent from expected. The optional entityIDs scope
// narrows only the unexpected set: when entityIDs is empty the scope is global
// (every present triple must be expected); otherwise only present triples whose
// entity_id is one of entityIDs are rejected. Missing expected triples are
// always checked globally, regardless of scope.
func AssertAuditCompletenessExact(t *testing.T, q Queryer, expected []Expectation, entityIDs ...string) {
	t.Helper()
	var scope map[string]struct{}
	if len(entityIDs) > 0 {
		scope = make(map[string]struct{}, len(entityIDs))
		for _, id := range entityIDs {
			scope[id] = struct{}{}
		}
	}
	checkAuditCompleteness(t, expected, loadAuditTriples(t, q), scope, true)
}

// loadAuditTriples reads every distinct audit triple once. It is a single
// SELECT with no arguments; the set lets both completeness modes share one
// consistent read.
func loadAuditTriples(t *testing.T, q Queryer) map[Expectation]struct{} {
	t.Helper()
	rows, err := q.Query(context.Background(),
		`SELECT action, entity_type, entity_id FROM audit_log`)
	if err != nil {
		t.Fatalf("testutil: query audit triples: %v", err)
	}
	defer rows.Close()

	actual := make(map[Expectation]struct{})
	for rows.Next() {
		var e Expectation
		if err := rows.Scan(&e.Action, &e.EntityType, &e.EntityID); err != nil {
			t.Fatalf("testutil: scan audit triple: %v", err)
		}
		actual[e] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("testutil: read audit triples: %v", err)
	}
	return actual
}

// checkAuditCompleteness is the unexported comparison seam. It emits exactly
// one Errorf containing every missing triple and, in exact mode, every
// unexpected in-scope triple. scope == nil means global.
func checkAuditCompleteness(t reporter, expected []Expectation, actual map[Expectation]struct{}, scope map[string]struct{}, exact bool) {
	t.Helper()

	expectedSet := make(map[Expectation]struct{}, len(expected))
	for _, e := range expected {
		expectedSet[e] = struct{}{}
	}

	missing := make([]Expectation, 0, len(expectedSet))
	for e := range expectedSet {
		if _, ok := actual[e]; !ok {
			missing = append(missing, e)
		}
	}
	sortExpectations(missing)

	var unexpected []Expectation
	if exact {
		for e := range actual {
			if _, ok := expectedSet[e]; ok {
				continue
			}
			if scope != nil {
				if _, inScope := scope[e.EntityID]; !inScope {
					continue
				}
			}
			unexpected = append(unexpected, e)
		}
		sortExpectations(unexpected)
	}

	if len(missing) == 0 && len(unexpected) == 0 {
		return
	}
	t.Errorf("%s", completenessFailure(missing, unexpected, len(expectedSet), exact))
}

// completenessFailure renders one deterministic message. Sections are sorted by
// entity type, entity ID, then action.
func completenessFailure(missing, unexpected []Expectation, expectedCount int, exact bool) string {
	var b strings.Builder
	if exact {
		fmt.Fprintf(&b, "testutil: audit completeness (exact): %d of %d expected audit triples missing, %d unexpected audit triples in scope",
			len(missing), expectedCount, len(unexpected))
	} else {
		fmt.Fprintf(&b, "testutil: audit completeness: %d of %d expected audit triples missing",
			len(missing), expectedCount)
	}
	if len(missing) > 0 {
		b.WriteString("\nmissing:")
		for _, e := range missing {
			b.WriteString("\n  - ")
			b.WriteString(e.String())
		}
	}
	if len(unexpected) > 0 {
		b.WriteString("\nunexpected in scope:")
		for _, e := range unexpected {
			b.WriteString("\n  - ")
			b.WriteString(e.String())
		}
	}
	return b.String()
}

// sortExpectations orders triples by entity type, entity ID, then action, as
// required by the completeness failure contract.
func sortExpectations(items []Expectation) {
	sort.Slice(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.EntityType != b.EntityType {
			return a.EntityType < b.EntityType
		}
		if a.EntityID != b.EntityID {
			return a.EntityID < b.EntityID
		}
		return a.Action < b.Action
	})
}
