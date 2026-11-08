package sandbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

const (
	defaultMaxConns int32 = 4
	maxRoleConns    int32 = 16
	adminPoolConns  int32 = 2
	cleanupTimeout        = 30 * time.Second
)

// templateEnsureMu serializes calls into internal/testdb.EnsureTemplate within
// this process: goose's embedded migration filesystem is process-global, so
// concurrent EnsureTemplate calls would otherwise race on goose.SetBaseFS even
// though the cross-process advisory lock keeps the database operations safe.
var templateEnsureMu sync.Mutex

// Options configures a Manager.
type Options struct {
	// AdminDSN is the maintenance DSN. Its role must have CREATEDB and
	// CREATEROLE (or be a superuser). It is never handed to the agent.
	AdminDSN string
	// Template is the template database name; it defaults to
	// DefaultTemplate.
	Template string
	// MaxConns caps each app/agent role pool. It defaults to 4; the runner
	// normally passes max(4, concurrency+2) capped at 16.
	MaxConns int32
	// BaseClock is the deterministic world/scenario fallback instant. It
	// defaults to dataset.DefaultGeneratedAt.
	BaseClock time.Time
	// Logger is optional; secrets and DSNs are never logged.
	Logger *slog.Logger
}

// Manager creates isolated eval database sandboxes.
type Manager struct {
	adminDSN  string
	template  string
	maxConns  int32
	baseClock time.Time
	logger    *slog.Logger

	ensureOnce    sync.Once
	ensureCreated bool
	ensureErr     error
}

// NewManager validates opts without connecting to Postgres.
func NewManager(opts Options) (*Manager, error) {
	if _, err := parseAdminDSN(opts.AdminDSN); err != nil {
		return nil, err
	}
	template := opts.Template
	if template == "" {
		template = DefaultTemplate
	}
	if template != DefaultTemplate && !validTemplateName(template) {
		return nil, fmt.Errorf("%w: invalid template name %q", ErrInvalidOptions, template)
	}
	maxConns := opts.MaxConns
	if maxConns == 0 {
		maxConns = defaultMaxConns
	}
	if maxConns < 0 {
		return nil, fmt.Errorf("%w: MaxConns must not be negative", ErrInvalidOptions)
	}
	if maxConns > maxRoleConns {
		maxConns = maxRoleConns
	}
	baseClock := opts.BaseClock
	if baseClock.IsZero() {
		baseClock = dataset.DefaultGeneratedAt
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		adminDSN:  opts.AdminDSN,
		template:  template,
		maxConns:  maxConns,
		baseClock: baseClock.UTC(),
		logger:    logger,
	}, nil
}

// validTemplateName keeps the override inside the same safe identifier and
// naming family as the default while allowing tests to isolate templates.
func validTemplateName(name string) bool {
	const prefix = "recon_eval_template"
	if len(name) <= len(prefix) || name[:len(prefix)] != prefix {
		return false
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

// EnsureTemplate creates, migrates, reuses, or rebuilds the eval template. It
// delegates to internal/testdb.EnsureTemplate and memoizes the first result per
// manager so concurrent Clone calls do not race.
func (m *Manager) EnsureTemplate(ctx context.Context) (bool, error) {
	m.ensureOnce.Do(func() {
		if err := m.checkAdminPrivileges(ctx); err != nil {
			m.ensureErr = err
			return
		}
		templateEnsureMu.Lock()
		m.ensureCreated, m.ensureErr = testdb.EnsureTemplate(ctx, testdb.Options{
			AdminDSN: m.adminDSN,
			Template: m.template,
		})
		templateEnsureMu.Unlock()
		if m.ensureErr != nil {
			m.ensureErr = fmt.Errorf("sandbox: ensure template %q: %w", m.template, m.ensureErr)
		}
	})
	return m.ensureCreated, m.ensureErr
}

// checkAdminPrivileges fails fast with a message naming the DSN capability
// instead of surfacing a raw permission-denied error from CREATE DATABASE or
// CREATE ROLE.
func (m *Manager) checkAdminPrivileges(ctx context.Context) error {
	conn, err := pgx.Connect(ctx, m.adminDSN)
	if err != nil {
		return fmt.Errorf("sandbox: connect admin database for privilege check: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	var (
		currentUser string
		superuser   bool
		createdb    bool
		createrole  bool
	)
	if err := conn.QueryRow(ctx,
		`SELECT current_user, rolsuper, rolcreatedb, rolcreaterole
		   FROM pg_roles WHERE rolname = current_user`).
		Scan(&currentUser, &superuser, &createdb, &createrole); err != nil {
		return fmt.Errorf("sandbox: inspect admin DSN role privileges: %w", err)
	}
	if superuser {
		return nil
	}
	var missing []string
	if !createdb {
		missing = append(missing, "CREATEDB")
	}
	if !createrole {
		missing = append(missing, "CREATEROLE")
	}
	if len(missing) > 0 {
		return fmt.Errorf(
			"sandbox: admin DSN role %q lacks %v; the eval maintenance DSN must be a superuser or have CREATEDB and CREATEROLE",
			currentUser, missing)
	}
	return nil
}

// Clone creates a fresh database from the template and loads ds into it. It is
// safe to call concurrently on one Manager and from several Managers.
func (m *Manager) Clone(ctx context.Context, ds *dataset.Dataset, load LoadOptions) (*Sandbox, error) {
	if err := validateCloneInputs(ds, load); err != nil {
		return nil, err
	}
	if _, err := m.EnsureTemplate(ctx); err != nil {
		return nil, err
	}

	name, err := nextDatabaseName()
	if err != nil {
		return nil, err
	}

	maintenance, err := pgx.Connect(ctx, m.adminDSN)
	if err != nil {
		return nil, fmt.Errorf("sandbox: connect maintenance database: %w", err)
	}
	_, createErr := maintenance.Exec(ctx,
		"CREATE DATABASE "+quoteIdent(name)+" TEMPLATE "+quoteIdent(m.template))
	if createErr != nil {
		_ = maintenance.Close(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("sandbox: create clone %q from template %q: %w", name, m.template, createErr)
	}

	s := &Sandbox{
		Name:     name,
		adminDSN: m.adminDSN,
		logger:   m.logger,
		base:     m.baseClock,
		logins:   make(map[string]loginUser, len(db.Roles())),
	}
	fail := func(opErr error) (*Sandbox, error) {
		_ = maintenance.Close(context.WithoutCancel(ctx))
		cleanupErr := s.Close(context.WithoutCancel(ctx))
		return nil, errors.Join(opErr, cleanupErr)
	}

	if err := provisionLogins(ctx, maintenance, m.adminDSN, name, s.logins); err != nil {
		return fail(fmt.Errorf("sandbox: provision ephemeral logins for %q: %w", name, err))
	}
	if err := maintenance.Close(context.WithoutCancel(ctx)); err != nil {
		return fail(fmt.Errorf("sandbox: close maintenance connection: %w", err))
	}

	adminDSN, err := dsnForDatabase(m.adminDSN, name)
	if err != nil {
		return fail(fmt.Errorf("sandbox: build clone admin DSN: %w", err))
	}
	s.Admin, err = openPool(ctx, adminDSN, adminPoolConns)
	if err != nil {
		return fail(fmt.Errorf("sandbox: open admin pool on %q: %w", name, err))
	}

	appLogin, ok := s.logins[db.RoleApp]
	if !ok {
		return fail(fmt.Errorf("sandbox: app login was not provisioned"))
	}
	s.appRole, err = openRolePool(ctx, db.RoleApp, appLogin.dsn, m.maxConns)
	if err != nil {
		return fail(fmt.Errorf("sandbox: open app role pool on %q: %w", name, err))
	}
	s.App = s.appRole.Pool

	agentLogin, ok := s.logins[db.RoleAgentRO]
	if !ok {
		return fail(fmt.Errorf("sandbox: agent login was not provisioned"))
	}
	s.agentRole, err = openRolePool(ctx, db.RoleAgentRO, agentLogin.dsn, m.maxConns)
	if err != nil {
		return fail(fmt.Errorf("sandbox: open agent role pool on %q: %w", name, err))
	}
	s.Agent = s.agentRole.Pool

	if err := s.loadWorld(ctx, ds, load); err != nil {
		return fail(fmt.Errorf("sandbox: load world into %q: %w", name, err))
	}
	m.logger.Debug("sandbox clone ready", "database", name, "template", m.template)
	return s, nil
}

// Close releases Manager resources. A Manager holds no connections between
// calls, so Close is a no-op kept for API symmetry.
func (m *Manager) Close(context.Context) error { return nil }

// Sandbox is one loaded eval world.
type Sandbox struct {
	Name  string
	Admin *pgxpool.Pool // setup + eval_truth only
	App   *pgxpool.Pool // recon_app on the clone
	Agent *pgxpool.Pool // recon_agent_ro read tools + proposal INSERT

	adminDSN  string
	logger    *slog.Logger
	base      time.Time
	logins    map[string]loginUser
	appRole   *db.RolePool
	agentRole *db.RolePool

	closeOnce sync.Once
	closeErr  error
}

// Close closes every pool, drops the clone with FORCE, and drops its ephemeral
// logins. It is idempotent and safe after a partial load failure. Teardown
// uses an uncanceled context so a canceled run still cleans up.
func (s *Sandbox) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.closeErr = s.close(ctx)
	})
	return s.closeErr
}

func (s *Sandbox) close(ctx context.Context) error {
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()

	var errs []error
	if s.appRole != nil {
		s.appRole.Close()
		s.appRole = nil
	}
	s.App = nil
	if s.agentRole != nil {
		s.agentRole.Close()
		s.agentRole = nil
	}
	s.Agent = nil
	if s.Admin != nil {
		s.Admin.Close()
		s.Admin = nil
	}

	conn, err := pgx.Connect(closeCtx, s.adminDSN)
	if err != nil {
		errs = append(errs, fmt.Errorf("sandbox: connect maintenance database for cleanup: %w", err))
		return errors.Join(errs...)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(closeCtx)) }()

	if s.Name != "" {
		if _, err := conn.Exec(closeCtx, "DROP DATABASE IF EXISTS "+quoteIdent(s.Name)+" WITH (FORCE)"); err != nil {
			errs = append(errs, fmt.Errorf("sandbox: drop clone %q: %w", s.Name, err))
		}
	}
	for _, login := range s.logins {
		if err := dropLoginWithConn(closeCtx, conn, login.Username); err != nil {
			errs = append(errs, err)
		}
	}
	s.logins = nil
	return errors.Join(errs...)
}

// openPool opens a pgxpool with an explicit connection cap.
func openPool(ctx context.Context, dsn string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("sandbox: parse DSN: %w", err)
	}
	cfg.MaxConns = maxConns
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("sandbox: create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("sandbox: ping pool: %w", err)
	}
	return pool, nil
}

// openRolePool opens a pool through db.OpenRole so a superuser or wrong
// membership DSN is rejected. Pool size is passed through the DSN because
// db.OpenRole owns the pgxpool configuration.
func openRolePool(ctx context.Context, role, dsn string, maxConns int32) (*db.RolePool, error) {
	withLimit, err := dsnWithMaxConns(dsn, maxConns)
	if err != nil {
		return nil, err
	}
	pool, err := db.OpenRole(ctx, role, withLimit)
	if err != nil {
		return nil, err
	}
	return pool, nil
}
