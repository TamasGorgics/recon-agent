package sandbox

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// CleanupOrphans drops every recon_eval_% database except template and returns
// the number dropped. It never touches recon_test_% or recon_test_template.
func CleanupOrphans(ctx context.Context, adminDSN, template string) (int, error) {
	if _, err := parseAdminDSN(adminDSN); err != nil {
		return 0, err
	}
	if template == "" {
		template = DefaultTemplate
	}
	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return 0, fmt.Errorf("sandbox: connect maintenance database: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	rows, err := conn.Query(ctx,
		`SELECT datname FROM pg_database
		  WHERE starts_with(datname, $1) AND datname <> $2
		  ORDER BY datname`, databasePrefix, template)
	if err != nil {
		return 0, fmt.Errorf("sandbox: list eval clone databases: %w", err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return 0, fmt.Errorf("sandbox: scan eval clone database: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("sandbox: list eval clone databases: %w", err)
	}
	rows.Close()

	dropped := 0
	var errs []error
	for _, name := range names {
		if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+quoteIdent(name)+" WITH (FORCE)"); err != nil {
			errs = append(errs, fmt.Errorf("sandbox: drop orphan clone %q: %w", name, err))
			continue
		}
		dropped++
	}
	return dropped, errors.Join(errs...)
}

// CleanupOrphanLogins drops every recon_eval_login_% role after terminating
// its backends and returns the number dropped. It never touches recon_test_%
// roles.
func CleanupOrphanLogins(ctx context.Context, adminDSN string) (int, error) {
	if _, err := parseAdminDSN(adminDSN); err != nil {
		return 0, err
	}
	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return 0, fmt.Errorf("sandbox: connect maintenance database: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	rows, err := conn.Query(ctx,
		`SELECT rolname FROM pg_roles
		  WHERE starts_with(rolname, $1)
		  ORDER BY rolname`, loginPrefix)
	if err != nil {
		return 0, fmt.Errorf("sandbox: list eval login roles: %w", err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return 0, fmt.Errorf("sandbox: scan eval login role: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("sandbox: list eval login roles: %w", err)
	}
	rows.Close()

	dropped := 0
	var errs []error
	for _, name := range names {
		if err := dropLoginWithConn(ctx, conn, name); err != nil {
			errs = append(errs, err)
			continue
		}
		dropped++
	}
	return dropped, errors.Join(errs...)
}
