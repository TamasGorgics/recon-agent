package testdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/TamasGorgics/recon-agent/migrations"
)

// DefaultTemplate is the template database cloned for every test.
const DefaultTemplate = "recon_test_template"

// DefaultMaxConns caps each per-test pool. Integration tests run with
// -parallel=4, which keeps four tests well below Postgres max_connections.
const DefaultMaxConns int32 = 4

// adminTimeout bounds maintenance operations that do not have a caller
// deadline, such as cleanup and template setup.
const adminTimeout = 30 * time.Second

// Options configures a test database.
type Options struct {
	// AdminDSN is the maintenance DSN. It defaults to TEST_DATABASE_URL and its
	// role must have CREATEDB and CREATEROLE (the compose default user is a
	// superuser) so it can clone databases and provision ephemeral role logins.
	AdminDSN string
	// Template is the template database name; it defaults to DefaultTemplate.
	Template string
	// MaxConns caps the per-test pool; it defaults to DefaultMaxConns.
	MaxConns int32
}

func (o Options) withDefaults() Options {
	if o.AdminDSN == "" {
		o.AdminDSN = os.Getenv("TEST_DATABASE_URL")
	}
	if o.Template == "" {
		o.Template = DefaultTemplate
	}
	if o.MaxConns <= 0 {
		o.MaxConns = DefaultMaxConns
	}
	return o
}

// DB is an isolated test database cloned from the template.
type DB struct {
	Name string
	DSN  string
	Pool *pgxpool.Pool

	adminDSN string
	logins   map[string]loginUser
}

var dbCounter atomic.Uint64

// DatabasePrefix returns the database-name prefix used for clones created by
// this process. The process id lets a package distinguish its own clones from
// clones created concurrently by other package test binaries, which share the
// recon_test_ namespace.
func DatabasePrefix() string {
	return fmt.Sprintf("recon_test_%d_", os.Getpid())
}

// New creates an isolated database from the default template using
// TEST_DATABASE_URL.
func New(t *testing.T) *DB {
	t.Helper()
	return NewWithOptions(t, Options{})
}

// NewWithOptions clones the configured template for one test. It fails fast
// when TEST_DATABASE_URL is unset or the template is missing. It registers a
// t.Cleanup that closes the pool and drops the clone with WITH (FORCE).
//
// The template must already exist; TestMain calls EnsureTemplate before
// recording its baseline so this path never creates it lazily.
func NewWithOptions(t *testing.T, opts Options) *DB {
	t.Helper()
	opts = opts.withDefaults()
	if opts.AdminDSN == "" {
		t.Fatalf("testdb: TEST_DATABASE_URL is required for integration tests; run via make test-integration or export it")
	}

	ctx, cancel := context.WithTimeout(context.Background(), adminTimeout)
	defer cancel()

	name := nextName()
	admin, err := pgx.Connect(ctx, opts.AdminDSN)
	if err != nil {
		t.Fatalf("testdb: connect admin database: %v", err)
	}
	_, cloneErr := admin.Exec(ctx, "CREATE DATABASE "+quoteIdent(name)+" TEMPLATE "+quoteIdent(opts.Template))
	_ = admin.Close(context.WithoutCancel(ctx))
	if cloneErr != nil {
		t.Fatalf("testdb: clone template %q: %v (does the template exist? TestMain must call EnsureTemplate)", opts.Template, cloneErr)
	}

	dsn, err := dsnForDatabase(opts.AdminDSN, name)
	if err != nil {
		_ = dropDatabase(ctx, opts.AdminDSN, name)
		t.Fatalf("testdb: build DSN for %q: %v", name, err)
	}

	d := &DB{Name: name, DSN: dsn, adminDSN: opts.AdminDSN, logins: make(map[string]loginUser)}
	t.Cleanup(func() { d.cleanup(t) })

	// Provision one ephemeral LOGIN per application role before returning. A
	// failure leaves the cleanup above to drop the clone and any logins created
	// so far.
	if err := d.provisionLogins(ctx); err != nil {
		t.Fatalf("testdb: provision ephemeral role logins for %q: %v", name, err)
	}

	pool, err := newPool(ctx, dsn, opts.MaxConns)
	if err != nil {
		t.Fatalf("testdb: open pool for %q: %v", name, err)
	}
	d.Pool = pool
	return d
}

// DSNFor returns the ephemeral DSN for role pointed at this test database.
// The empty role returns the maintenance DSN. An unknown role panics: falling
// back to the maintenance DSN would silently escalate the caller's privileges.
func (d *DB) DSNFor(role string) string {
	if role == "" {
		return d.DSN
	}
	canonical, ok := canonicalRole(role)
	if !ok {
		panic(fmt.Sprintf("testdb: DSNFor(%q): unknown role; use one of %s", role, strings.Join(RoleNames(), ", ")))
	}
	login, ok := d.logins[canonical]
	if !ok {
		panic(fmt.Sprintf("testdb: DSNFor(%q): no ephemeral login provisioned", role))
	}
	return login.dsn
}

// LoginNames returns the ephemeral LOGIN role names provisioned for this test
// database, sorted. It is used by cleanup assertions in tests.
func (d *DB) LoginNames() []string {
	names := make([]string, 0, len(d.logins))
	for _, login := range d.logins {
		names = append(names, login.Username)
	}
	sort.Strings(names)
	return names
}

// EnsureTemplate creates and migrates the template database when it is absent,
// reuses it when it is at the current migration version, and drops/recreates it
// when its goose version is older, newer, or missing relative to migrations.FS.
// The returned bool reports whether this call created (or recreated) it.
//
// Call it from TestMain before recording the database baseline: the ADV-01
// lifecycle requires the template to exist before m.Run so the orphan
// assertion sees a stable baseline and a second run reuses the template.
func EnsureTemplate(ctx context.Context, opts Options) (bool, error) {
	opts = opts.withDefaults()
	if opts.AdminDSN == "" {
		return false, errors.New("testdb: TEST_DATABASE_URL is required for integration tests")
	}

	currentVersion, err := currentMigrationVersion()
	if err != nil {
		return false, fmt.Errorf("testdb: determine current migration version: %w", err)
	}

	admin, err := pgx.Connect(ctx, opts.AdminDSN)
	if err != nil {
		return false, fmt.Errorf("testdb: connect admin database: %w", err)
	}
	defer func() { _ = admin.Close(context.WithoutCancel(ctx)) }()

	// Serialize template setup across concurrent test processes. The lock is
	// session-scoped and released when the admin connection closes.
	if _, err := admin.Exec(ctx, "SELECT pg_advisory_lock($1)", templateLockKey); err != nil {
		return false, fmt.Errorf("testdb: lock template setup: %w", err)
	}
	defer func() {
		_, _ = admin.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", templateLockKey)
	}()

	var exists bool
	if err := admin.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", opts.Template).Scan(&exists); err != nil {
		return false, fmt.Errorf("testdb: check template %q: %w", opts.Template, err)
	}
	if exists {
		templateVersion, hasVersionTable, err := templateMigrationVersion(ctx, opts.AdminDSN, opts.Template)
		if err != nil {
			return false, fmt.Errorf("testdb: read template %q migration version: %w", opts.Template, err)
		}
		if hasVersionTable && templateVersion == currentVersion {
			return false, nil
		}
		if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+quoteIdent(opts.Template)+" WITH (FORCE)"); err != nil {
			return false, fmt.Errorf("testdb: drop stale template %q (version table=%t, version=%d, want %d): %w",
				opts.Template, hasVersionTable, templateVersion, currentVersion, err)
		}
	}

	if _, err := admin.Exec(ctx, "CREATE DATABASE "+quoteIdent(opts.Template)); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42P04" { // duplicate_database: a concurrent run won the race
			return false, nil
		}
		return false, fmt.Errorf("testdb: create template %q: %w", opts.Template, err)
	}
	if err := migrateTemplate(ctx, opts.AdminDSN, opts.Template); err != nil {
		// Never leave a half-migrated template for the next run.
		_, _ = admin.Exec(context.WithoutCancel(ctx), "DROP DATABASE IF EXISTS "+quoteIdent(opts.Template)+" WITH (FORCE)")
		return false, fmt.Errorf("testdb: migrate template %q: %w", opts.Template, err)
	}
	return true, nil
}

// CleanupOrphans drops every recon_test_% database except the template. It is
// idempotent and returns the number of databases it dropped.
func CleanupOrphans(ctx context.Context, adminDSN string) (int, error) {
	if adminDSN == "" {
		return 0, errors.New("testdb: admin DSN is required")
	}

	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return 0, fmt.Errorf("testdb: connect admin database: %w", err)
	}
	defer func() { _ = admin.Close(context.WithoutCancel(ctx)) }()

	rows, err := admin.Query(ctx,
		`SELECT datname FROM pg_database WHERE starts_with(datname, 'recon_test_') AND datname <> $1 ORDER BY datname`,
		DefaultTemplate)
	if err != nil {
		return 0, fmt.Errorf("testdb: list test databases: %w", err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return 0, fmt.Errorf("testdb: scan test database: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("testdb: list test databases: %w", err)
	}
	rows.Close()

	dropped := 0
	for _, name := range names {
		if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+quoteIdent(name)+" WITH (FORCE)"); err != nil {
			return dropped, fmt.Errorf("testdb: drop database %q: %w", name, err)
		}
		dropped++
	}
	return dropped, nil
}

// currentMigrationVersion returns the highest migration version embedded in
// migrations.FS. It mirrors the version goose records in goose_db_version.
func currentMigrationVersion() (int64, error) {
	goose.SetBaseFS(migrations.FS)
	found, err := goose.CollectMigrations(".", 0, goose.MaxVersion)
	if err != nil {
		return 0, fmt.Errorf("collect migrations: %w", err)
	}
	var maxVersion int64
	for _, migration := range found {
		if migration.Version > maxVersion {
			maxVersion = migration.Version
		}
	}
	if maxVersion == 0 {
		return 0, errors.New("no migrations embedded in migrations.FS")
	}
	return maxVersion, nil
}

// templateMigrationVersion reads the template's applied goose version. A
// missing goose_db_version table means the template predates the migration
// system (or was created by hand), so it must be rebuilt.
func templateMigrationVersion(ctx context.Context, adminDSN, template string) (version int64, hasTable bool, err error) {
	dsn, err := dsnForDatabase(adminDSN, template)
	if err != nil {
		return 0, false, err
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return 0, false, fmt.Errorf("connect template database: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	if err := conn.QueryRow(ctx, "SELECT COALESCE(max(version_id), 0) FROM goose_db_version").Scan(&version); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42P01" { // undefined_table
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("query goose_db_version: %w", err)
	}
	return version, true, nil
}

// templateLockKey is an arbitrary advisory-lock key for template setup.
const templateLockKey int64 = 0x7265636f6e // "recon"

func nextName() string {
	return fmt.Sprintf("%s%d_%s",
		DatabasePrefix(), dbCounter.Add(1), strconv.FormatUint(uint64(rand.Uint32()), 36))
}

func newPool(ctx context.Context, dsn string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	cfg.MaxConns = maxConns
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}

func (d *DB) cleanup(t *testing.T) {
	t.Helper()
	if d.Pool != nil {
		d.Pool.Close()
		d.Pool = nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), adminTimeout)
	defer cancel()
	if err := dropDatabase(ctx, d.adminDSN, d.Name); err != nil {
		t.Errorf("testdb: drop database %q: %v", d.Name, err)
	}
	for _, login := range d.logins {
		if err := dropLogin(ctx, d.adminDSN, login.Username); err != nil {
			t.Errorf("testdb: drop ephemeral login %q: %v", login.Username, err)
		}
	}
}

func dropDatabase(ctx context.Context, adminDSN, name string) error {
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return fmt.Errorf("connect admin database: %w", err)
	}
	defer func() { _ = admin.Close(context.WithoutCancel(ctx)) }()
	if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+quoteIdent(name)+" WITH (FORCE)"); err != nil {
		return fmt.Errorf("drop: %w", err)
	}
	return nil
}

// migrateTemplate applies the embedded goose migrations to the template. The
// database/sql pool is closed before this returns so CREATE DATABASE ... TEMPLATE
// never sees an active connection to the template.
func migrateTemplate(ctx context.Context, adminDSN, template string) error {
	dsn, err := dsnForDatabase(adminDSN, template)
	if err != nil {
		return err
	}
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open template database: %w", err)
	}
	defer func() { _ = sqlDB.Close() }()
	if err := sqlDB.PingContext(ctx); err != nil {
		return fmt.Errorf("connect template database: %w", err)
	}
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("configure goose: %w", err)
	}
	if err := goose.UpContext(ctx, sqlDB, "."); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

func dsnForDatabase(dsn, database string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("testdb: parse dsn: %w", err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return "", fmt.Errorf("testdb: admin DSN must be a postgres:// URL, got scheme %q", u.Scheme)
	}
	u.Path = "/" + database
	u.RawPath = ""
	return u.String(), nil
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
