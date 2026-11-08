package sandbox

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/db"
)

// loginUser is one ephemeral LOGIN role provisioned for a clone. dsn carries a
// random password and must never be logged.
type loginUser struct {
	Role     string
	Username string
	dsn      string
}

// provisionLogins creates one LOGIN per application group role, all members of
// their group. The maintenance connection must be a superuser or own the group
// roles with ADMIN OPTION. Partial creation is recorded in logins so the
// caller's cleanup can drop what was created.
func provisionLogins(ctx context.Context, admin *pgx.Conn, adminDSN, database string,
	logins map[string]loginUser) error {
	for _, role := range db.Roles() {
		username, err := nextLoginName(role)
		if err != nil {
			return err
		}
		password, err := randomToken(24)
		if err != nil {
			return fmt.Errorf("sandbox: generate password for %s: %w", role, err)
		}
		stmt := fmt.Sprintf(
			"CREATE ROLE %s LOGIN INHERIT PASSWORD %s IN ROLE %s",
			quoteIdent(username), quoteLiteral(password), quoteIdent(role))
		if _, err := admin.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("sandbox: create ephemeral login for %s: %w", role, err)
		}
		dsn, err := dsnForLogin(adminDSN, database, username, password)
		if err != nil {
			// Record the role so cleanup drops it even though the DSN failed.
			logins[role] = loginUser{Role: role, Username: username}
			return fmt.Errorf("sandbox: build DSN for ephemeral login: %w", err)
		}
		logins[role] = loginUser{Role: role, Username: username, dsn: dsn}
	}
	return nil
}

// dropLoginWithConn terminates leftover backends owned by username and drops
// the role. The connection must point at a database other than one being
// dropped.
func dropLoginWithConn(ctx context.Context, conn *pgx.Conn, username string) error {
	if username == "" {
		return nil
	}
	if _, err := conn.Exec(ctx,
		`SELECT pg_terminate_backend(pid)
		   FROM pg_stat_activity
		  WHERE usename = $1 AND pid <> pg_backend_pid()`, username); err != nil {
		return fmt.Errorf("sandbox: terminate backends for login %q: %w", username, err)
	}
	if _, err := conn.Exec(ctx, "DROP ROLE IF EXISTS "+quoteIdent(username)); err != nil {
		return fmt.Errorf("sandbox: drop ephemeral login %q: %w", username, err)
	}
	return nil
}
