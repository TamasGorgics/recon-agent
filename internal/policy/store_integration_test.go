//go:build integration

// covers: FIN-13, FIN-34
package policy_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/policy"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

// TestPolicyConfigVersionIntegration proves content-addressed versioning
// through the real recon_app login: first insert, idempotent no-op, exact
// stored-YAML verification, injected microsecond loaded_at, and append-only
// grants.
func TestPolicyConfigVersionIntegration(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	app := openRolePool(t, d, db.RoleApp)

	loaded, err := policy.LoadFile("../../policy.yaml")
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	loadedAt := time.Date(2026, 11, 12, 12, 34, 56, 123456789, time.UTC)
	wantLoadedAt := time.Date(2026, 11, 12, 12, 34, 56, 123456000, time.UTC)

	if err := db.WithTx(ctx, app, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return loaded.EnsureVersion(ctx, tx, loadedAt)
	}); err != nil {
		t.Fatalf("first EnsureVersion: %v", err)
	}

	var storedYAML string
	var storedAt time.Time
	if err := app.QueryRow(ctx,
		`SELECT yaml, loaded_at FROM policy_config_versions WHERE hash = $1`,
		loaded.Hash).Scan(&storedYAML, &storedAt); err != nil {
		t.Fatalf("read stored policy version: %v", err)
	}
	if storedYAML != string(loaded.YAML) {
		t.Fatal("stored YAML differs from the loaded bytes")
	}
	if !storedAt.Equal(wantLoadedAt) {
		t.Fatalf("loaded_at = %s, want truncated %s", storedAt, wantLoadedAt)
	}

	// A second insert is an idempotent no-op and never rewrites loaded_at.
	later := loadedAt.Add(24 * time.Hour)
	if err := db.WithTx(ctx, app, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return loaded.EnsureVersion(ctx, tx, later)
	}); err != nil {
		t.Fatalf("second EnsureVersion: %v", err)
	}
	var count int
	var secondAt time.Time
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*), max(loaded_at) FROM policy_config_versions WHERE hash = $1`,
		loaded.Hash).Scan(&count, &secondAt); err != nil {
		t.Fatalf("count policy versions: %v", err)
	}
	if count != 1 {
		t.Fatalf("policy_config_versions rows = %d, want 1", count)
	}
	if !secondAt.Equal(wantLoadedAt) {
		t.Fatalf("second EnsureVersion rewrote loaded_at to %s, want %s", secondAt, wantLoadedAt)
	}

	// Invalid version inputs fail closed before any SQL.
	if err := db.WithTx(ctx, app, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		if err := loaded.EnsureVersion(ctx, tx, time.Time{}); !errors.Is(err, policy.ErrInvalidVersionInput) {
			t.Fatalf("zero loadedAt error = %v, want ErrInvalidVersionInput", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("zero loadedAt transaction: %v", err)
	}
	if err := loaded.EnsureVersion(ctx, nil, loadedAt); !errors.Is(err, policy.ErrInvalidVersionInput) {
		t.Fatalf("nil tx error = %v, want ErrInvalidVersionInput", err)
	}

	// A hash that already maps to different bytes fails closed and is never
	// overwritten. Use a second document so the clean first insert above is
	// not disturbed.
	variant, err := policy.LoadBytes(append(append([]byte(nil), loaded.YAML...), []byte("# variant\n")...))
	if err != nil {
		t.Fatalf("LoadBytes variant: %v", err)
	}
	if variant.Hash == loaded.Hash {
		t.Fatal("variant document has the same hash, want a new version")
	}
	tamperedYAML := "schema_version: 1\n# tampered\n"
	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO policy_config_versions (hash, yaml, loaded_at)
		 VALUES ($1, $2, $3)`, variant.Hash, tamperedYAML, loadedAt); err != nil {
		t.Fatalf("seed tampered version: %v", err)
	}
	if err := db.WithTx(ctx, app, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		err := variant.EnsureVersion(ctx, tx, loadedAt.Add(time.Hour))
		if !errors.Is(err, policy.ErrConfigHashMismatch) {
			return err
		}
		return nil
	}); err != nil {
		t.Fatalf("tampered EnsureVersion: %v", err)
	}
	if err := d.Pool.QueryRow(ctx,
		`SELECT yaml FROM policy_config_versions WHERE hash = $1`, variant.Hash).
		Scan(&storedYAML); err != nil {
		t.Fatalf("re-read tampered version: %v", err)
	}
	if storedYAML != tamperedYAML {
		t.Fatal("EnsureVersion overwrote a mismatched stored policy version")
	}

	// The app can SELECT/INSERT policy versions but never UPDATE/DELETE them.
	for _, tc := range []struct {
		name string
		sql  string
		args []any
	}{
		{"update", `UPDATE policy_config_versions SET yaml = 'schema_version: 1' WHERE hash = $1`, []any{loaded.Hash}},
		{"delete", `DELETE FROM policy_config_versions WHERE hash = $1`, []any{loaded.Hash}},
		{"upsert", `INSERT INTO policy_config_versions (hash, yaml, loaded_at)
		            VALUES ($1, 'schema_version: 1', now())
		            ON CONFLICT (hash) DO UPDATE SET yaml = 'schema_version: 1'`, []any{loaded.Hash}},
	} {
		tc := tc
		t.Run("deny_"+tc.name, func(t *testing.T) {
			if _, err := app.Exec(ctx, tc.sql, tc.args...); err == nil {
				t.Fatalf("app %s policy_config_versions succeeded, want permission denied", tc.name)
			} else {
				requirePgError(t, err, sqlStateInsufficientPrivilege)
			}
		})
	}
}

const sqlStateInsufficientPrivilege = "42501"

func requirePgError(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected SQLSTATE %s, got nil", code)
	}
	type pgError interface{ SQLState() string }
	var pgErr pgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("error %v does not expose SQLSTATE", err)
	}
	if pgErr.SQLState() != code {
		t.Fatalf("SQLSTATE = %s, want %s (error: %v)", pgErr.SQLState(), code, err)
	}
}
