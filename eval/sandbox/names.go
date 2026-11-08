package sandbox

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
)

const (
	// DefaultTemplate is the template database cloned for every eval sandbox.
	DefaultTemplate = "recon_eval_template"

	databasePrefix = "recon_eval_"
	loginPrefix    = "recon_eval_login_"
)

var (
	databaseCounter atomic.Uint64
	loginCounter    atomic.Uint64
)

// nextDatabaseName returns recon_eval_<pid>_<counter>_<random>. The
// recon_eval_ prefix is deliberately disjoint from internal/testdb's
// recon_test_% namespace so the test cleanup can never touch eval clones.
func nextDatabaseName() (string, error) {
	suffix, err := randomToken(6)
	if err != nil {
		return "", fmt.Errorf("sandbox: generate database suffix: %w", err)
	}
	return fmt.Sprintf("%s%d_%d_%s", databasePrefix, os.Getpid(), databaseCounter.Add(1), suffix), nil
}

// nextLoginName returns recon_eval_login_<pid>_<counter>_<random>_<suffix>.
func nextLoginName(role string) (string, error) {
	suffix, err := randomToken(4)
	if err != nil {
		return "", fmt.Errorf("sandbox: generate login suffix: %w", err)
	}
	return fmt.Sprintf("%s%d_%d_%s_%s", loginPrefix, os.Getpid(), loginCounter.Add(1), suffix,
		strings.TrimPrefix(role, "recon_")), nil
}

// randomToken returns hex-encoded random bytes from crypto/rand.
func randomToken(size int) (string, error) {
	if size <= 0 {
		return "", fmt.Errorf("sandbox: random token size must be positive")
	}
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// quoteIdent quotes a Postgres identifier. Names are generated locally but
// the quote keeps the SQL safe and explicit.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// quoteLiteral quotes a Postgres string literal.
func quoteLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
