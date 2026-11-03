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
