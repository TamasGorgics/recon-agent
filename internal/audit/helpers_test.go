//go:build integration

package audit_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
	"github.com/TamasGorgics/recon-agent/internal/testutil"
)

const (
	sqlStateInsufficientPrivilege = "42501"
	sqlStateRestrictViolation     = "23001"
)

func mustActor(t *testing.T, canonical string) audit.Actor {
	t.Helper()
	actor, err := audit.ParseActor(canonical)
	if err != nil {
		t.Fatalf("parse actor %q: %v", canonical, err)
	}
	return actor
}

func ptr(s string) *string { return &s }

// chainEntry returns a deterministic, fully populated entry. Entries differ by
// n so a chain of them exercises every hashed field.
func chainEntry(t *testing.T, n int) audit.Entry {
	t.Helper()
	return audit.Entry{
		Actor:      mustActor(t, "system:matcher"),
		Action:     fmt.Sprintf("test.action.%02d", n),
		EntityType: "audit_test",
		EntityID:   fmt.Sprintf("entity-%02d", n),
		Before: map[string]any{
			"amount_minor": int64(n * 100),
			"id":           fmt.Sprintf("before-%02d", n),
		},
		After: map[string]any{
			"amount_minor": int64(n * 200),
			"id":           fmt.Sprintf("after-%02d", n),
		},
		TraceID:   ptr(fmt.Sprintf("trace-%02d", n)),
		Timestamp: time.Date(2026, 11, 4, 12, 0, n, 123456000, time.UTC),
	}
}

func appendCommitted(t *testing.T, ctx context.Context, b db.Beginner, e audit.Entry) audit.Record {
	t.Helper()
	return testutil.AppendAudit(t, ctx, b, e)
}

type storedRow struct {
	Seq        int64
	TS         time.Time
	ActorType  string
	ActorID    string
	Action     string
	EntityType string
	EntityID   string
	Before     pgtype.Text
	After      pgtype.Text
	TraceID    pgtype.Text
	PrevHash   []byte
	RowHash    []byte
}

func readStoredRow(t *testing.T, ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, seq int64) storedRow {
	t.Helper()
	var row storedRow
	err := q.QueryRow(ctx,
		`SELECT seq, ts, actor_type, actor_id, action, entity_type, entity_id,
		        before::text, after::text, trace_id, prev_hash, row_hash
		   FROM audit_log WHERE seq = $1`, seq).
		Scan(&row.Seq, &row.TS, &row.ActorType, &row.ActorID, &row.Action,
			&row.EntityType, &row.EntityID, &row.Before, &row.After, &row.TraceID,
			&row.PrevHash, &row.RowHash)
	if err != nil {
		t.Fatalf("read audit row %d: %v", seq, err)
	}
	return row
}

func countAuditRows(t *testing.T, ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, table string) int64 {
	t.Helper()
	var count int64
	if err := q.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return count
}

func openRolePool(t *testing.T, d *testdb.DB, role string) *pgxpool.Pool {
	t.Helper()
	rp, err := db.OpenRole(context.Background(), role, d.DSNFor(role))
	if err != nil {
		t.Fatalf("open %s pool: %v", role, err)
	}
	t.Cleanup(rp.Close)
	return rp.Pool
}

func requirePgError(t *testing.T, err error, codes ...string) *pgconn.PgError {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a Postgres error (%s), got nil", strings.Join(codes, ", "))
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("error %v is not a *pgconn.PgError", err)
	}
	for _, code := range codes {
		if pgErr.Code == code {
			return pgErr
		}
	}
	t.Fatalf("SQLSTATE = %s, want one of %v (error: %v)", pgErr.Code, codes, err)
	return nil
}

func hasProblem(report audit.Report, kind audit.ProblemKind, seq int64) bool {
	for _, problem := range report.Problems {
		if problem.Kind == kind && (seq == 0 || problem.Seq == seq) {
			return true
		}
	}
	return false
}

func hasAnyProblem(report audit.Report, kinds ...audit.ProblemKind) bool {
	for _, problem := range report.Problems {
		for _, kind := range kinds {
			if problem.Kind == kind {
				return true
			}
		}
	}
	return false
}
