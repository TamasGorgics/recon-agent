// Package testutil provides helpers shared by integration tests across
// packages. It must never be imported by production code.
package testutil

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/db"
)

// Queryer is the subset of pgx used by audit assertions. It is satisfied by
// *pgxpool.Pool and pgx.Tx, so callers can assert inside or after a transaction.
type Queryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
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
