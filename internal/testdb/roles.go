package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync/atomic"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/db"
)

// Role names keep test code and the db package's role-pool helper in sync.
const (
	RoleApp      = db.RoleApp
	RoleExecutor = db.RoleExecutor
	RoleAgentRO  = db.RoleAgentRO
	RoleAuditRO  = db.RoleAuditRO
)

// loginRoleNamePrefix scopes ephemeral login names to one test process. The
// process id makes the per-package orphan assertions independent when Go runs
// several package test binaries in parallel.
const loginRoleNamePrefix = "recon_test_login_"

var loginCounter atomic.Uint64

// RoleNames returns the four roles, in stable order.
func RoleNames() []string {
	return db.Roles()
}

// LoginRolePrefix returns the name prefix used by ephemeral LOGIN roles created
// by this process. Tests use it to assert no login role survives cleanup.
func LoginRolePrefix() string {
	return fmt.Sprintf("%s%d_", loginRoleNamePrefix, os.Getpid())
}

// loginUser is one ephemeral login provisioned for a test database. dsn carries
// a random password; it must not be logged.
type loginUser struct {
	Role     string
	Username string
	dsn      string
}

func canonicalRole(role string) (string, bool) {
	switch role {
	case RoleApp, "app":
		return RoleApp, true
	case RoleExecutor, "executor":
		return RoleExecutor, true
	case RoleAgentRO, "agent_ro":
		return RoleAgentRO, true
	case RoleAuditRO, "audit_ro":
		return RoleAuditRO, true
	default:
		return "", false
	}
}

// provisionLogins creates one LOGIN user per application role, all members of
// their group role, and records the DSNs on the DB. The maintenance connection
// must be a superuser (or own the group roles with ADMIN OPTION).
func (d *DB) provisionLogins(ctx context.Context) error {
	if d.logins == nil {
		d.logins = make(map[string]loginUser, len(RoleNames()))
	}

	admin, err := pgx.Connect(ctx, d.adminDSN)
	if err != nil {
		return fmt.Errorf("testdb: connect admin database: %w", err)
	}
	defer func() { _ = admin.Close(context.WithoutCancel(ctx)) }()

	for _, role := range RoleNames() {
		username := nextLoginName(role)
		password, err := randomToken(24)
		if err != nil {
			return fmt.Errorf("testdb: generate password for %s: %w", role, err)
		}
		stmt := fmt.Sprintf(
			"CREATE ROLE %s LOGIN INHERIT PASSWORD %s IN ROLE %s",
			quoteIdent(username), quoteLiteral(password), quoteIdent(role))
		if _, err := admin.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("testdb: create ephemeral login %q for %s: %w", username, role, err)
		}

		dsn, err := dsnForLogin(d.adminDSN, d.Name, username, password)
		if err != nil {
			_ = dropLoginWithConn(ctx, admin, username)
			return fmt.Errorf("testdb: build DSN for ephemeral login %q: %w", username, err)
		}
		d.logins[role] = loginUser{Role: role, Username: username, dsn: dsn}
	}
	return nil
}

func nextLoginName(role string) string {
	suffix := strings.TrimPrefix(role, "recon_")
	token, err := randomToken(4)
	if err != nil {
		// crypto/rand failure is unrecoverable; a predictable suffix is better
		// than a panic and the password remains random.
		token = "fallback"
	}
	return fmt.Sprintf("%s%d_%s_%s", LoginRolePrefix(), loginCounter.Add(1), token, suffix)
}

func randomToken(size int) (string, error) {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// dsnForLogin rewrites the maintenance DSN to point at one test database and
// authenticate as username with password.
func dsnForLogin(dsn, database, username, password string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("testdb: parse dsn: %w", err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return "", fmt.Errorf("testdb: admin DSN must be a postgres:// URL, got scheme %q", u.Scheme)
	}
	u.Path = "/" + database
	u.RawPath = ""
	u.User = url.UserPassword(username, password)
	return u.String(), nil
}

// dropLogin terminates leftover backends owned by username and drops the role.
func dropLogin(ctx context.Context, adminDSN, username string) error {
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return fmt.Errorf("testdb: connect admin database: %w", err)
	}
	defer func() { _ = admin.Close(context.WithoutCancel(ctx)) }()
	return dropLoginWithConn(ctx, admin, username)
}

func dropLoginWithConn(ctx context.Context, conn *pgx.Conn, username string) error {
	// A test pool that was not closed would otherwise keep the role in use.
	_, _ = conn.Exec(ctx,
		`SELECT pg_terminate_backend(pid)
		   FROM pg_stat_activity
		  WHERE usename = $1 AND pid <> pg_backend_pid()`, username)
	if _, err := conn.Exec(ctx, "DROP ROLE IF EXISTS "+quoteIdent(username)); err != nil {
		return fmt.Errorf("testdb: drop ephemeral login %q: %w", username, err)
	}
	return nil
}

// CleanupOrphanLogins drops every recon_test_% login role. It is for
// maintenance (`recon testdb clean`), not for use while tests are running.
func CleanupOrphanLogins(ctx context.Context, adminDSN string) (int, error) {
	if adminDSN == "" {
		return 0, errors.New("testdb: admin DSN is required")
	}
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return 0, fmt.Errorf("testdb: connect admin database: %w", err)
	}
	defer func() { _ = admin.Close(context.WithoutCancel(ctx)) }()

	rows, err := admin.Query(ctx,
		`SELECT rolname FROM pg_roles WHERE starts_with(rolname, 'recon_test_') ORDER BY rolname`)
	if err != nil {
		return 0, fmt.Errorf("testdb: list ephemeral login roles: %w", err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return 0, fmt.Errorf("testdb: scan ephemeral login role: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("testdb: list ephemeral login roles: %w", err)
	}
	rows.Close()

	dropped := 0
	for _, name := range names {
		if err := dropLoginWithConn(ctx, admin, name); err != nil {
			return dropped, err
		}
		dropped++
	}
	return dropped, nil
}

func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
