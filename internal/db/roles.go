package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The four NOLOGIN group roles from spec section 16 and M1 spec D-6. Logins
// are provisioned outside migrations: compose creates local-only development
// users, and integration tests create ephemeral users per test database.
const (
	RoleApp      = "recon_app"
	RoleExecutor = "recon_executor"
	RoleAgentRO  = "recon_agent_ro"
	RoleAuditRO  = "recon_audit_ro"
)

// Errors returned by OpenRole. They are sentinels so callers (and the role
// matrix test) can distinguish a typo from a connection or privilege problem.
var (
	// ErrUnknownRole means the name is not one of the four application roles.
	ErrUnknownRole = errors.New("db: unknown role")
	// ErrSuperuserRolePool means the DSN connects as a superuser. A role pool
	// that silently used maintenance privileges would hide grant regressions.
	ErrSuperuserRolePool = errors.New("db: role pool must not connect as a superuser")
	// ErrRoleNotMember means the session user is not a member of the named role.
	ErrRoleNotMember = errors.New("db: session user is not a member of the role")
)

// Roles returns the four application roles in a stable order.
func Roles() []string {
	return []string{RoleApp, RoleExecutor, RoleAgentRO, RoleAuditRO}
}

// IsRole reports whether name is one of the four application roles.
func IsRole(name string) bool {
	for _, role := range Roles() {
		if name == role {
			return true
		}
	}
	return false
}

// RolePool is a pool opened through a role login. Role names the group the
// session must be a member of; Pool is the underlying pgx pool.
type RolePool struct {
	Role string
	Pool *pgxpool.Pool
}

// Close closes the underlying pool. It is safe to call on a nil RolePool.
func (p *RolePool) Close() {
	if p != nil && p.Pool != nil {
		p.Pool.Close()
	}
}

// OpenRole opens a pool for one of the four application roles. It rejects
// unknown role names, superuser sessions, and sessions that are not members of
// the named role. Those checks keep the role matrix honest: a test cannot pass
// by accidentally connecting with the maintenance DSN.
func OpenRole(ctx context.Context, name, dsn string) (*RolePool, error) {
	if !IsRole(name) {
		return nil, fmt.Errorf("%w: %q", ErrUnknownRole, name)
	}

	cfg, err := parsePoolConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("db: parse %s dsn: %w", name, err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db: create %s pool: %w", name, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping %s pool: %w", name, err)
	}

	var currentUser, superuser string
	var member bool
	if err := pool.QueryRow(ctx,
		`SELECT current_user, current_setting('is_superuser'), pg_has_role(current_user, $1::name, 'MEMBER')`,
		name).Scan(&currentUser, &superuser, &member); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: inspect %s session: %w", name, err)
	}
	if superuser == "on" {
		pool.Close()
		return nil, fmt.Errorf("%w: %s connected as %q", ErrSuperuserRolePool, name, currentUser)
	}
	if !member {
		pool.Close()
		return nil, fmt.Errorf("%w: %s session is %q", ErrRoleNotMember, name, currentUser)
	}

	return &RolePool{Role: name, Pool: pool}, nil
}
