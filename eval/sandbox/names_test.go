package sandbox

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/db"
)

func TestDatabaseAndLoginNames(t *testing.T) {
	name, err := nextDatabaseName()
	if err != nil {
		t.Fatalf("nextDatabaseName: %v", err)
	}
	databasePattern := regexp.MustCompile(fmt.Sprintf(`^recon_eval_%d_\d+_[0-9a-f]{12}$`, os.Getpid()))
	if !databasePattern.MatchString(name) {
		t.Fatalf("database name %q does not match recon_eval_<pid>_<counter>_<random>", name)
	}
	if strings.HasPrefix(name, "recon_test_") {
		t.Fatalf("eval database name %q is in the test namespace", name)
	}

	loginPattern := regexp.MustCompile(
		fmt.Sprintf(`^recon_eval_login_%d_\d+_[0-9a-f]{8}_(app|executor|agent_ro|audit_ro)$`, os.Getpid()))
	for _, role := range db.Roles() {
		login, err := nextLoginName(role)
		if err != nil {
			t.Fatalf("nextLoginName(%s): %v", role, err)
		}
		if !loginPattern.MatchString(login) {
			t.Fatalf("login name %q does not match recon_eval_login_*", login)
		}
	}
}

func TestDSNHelpers(t *testing.T) {
	admin := "postgres://user:secret@localhost:5432/recon?sslmode=disable"
	databaseDSN, err := dsnForDatabase(admin, "recon_eval_1")
	if err != nil {
		t.Fatalf("dsnForDatabase: %v", err)
	}
	if !strings.Contains(databaseDSN, "/recon_eval_1?") ||
		!strings.Contains(databaseDSN, "sslmode=disable") {
		t.Fatalf("database DSN lost path or query: %s", databaseDSN)
	}
	loginDSN, err := dsnForLogin(admin, "recon_eval_1", "recon_eval_login_x", "p@ss word")
	if err != nil {
		t.Fatalf("dsnForLogin: %v", err)
	}
	if !strings.Contains(loginDSN, "recon_eval_login_x") || !strings.Contains(loginDSN, "/recon_eval_1") {
		t.Fatalf("login DSN missing user or database: %s", loginDSN)
	}
	limited, err := dsnWithMaxConns(databaseDSN, 7)
	if err != nil {
		t.Fatalf("dsnWithMaxConns: %v", err)
	}
	if !strings.Contains(limited, "pool_max_conns=7") {
		t.Fatalf("pool_max_conns missing: %s", limited)
	}
	if _, err := NewManager(Options{AdminDSN: "http://localhost/x"}); err == nil {
		t.Fatal("NewManager accepted a non-postgres DSN")
	}
	if _, err := NewManager(Options{AdminDSN: ""}); err == nil {
		t.Fatal("NewManager accepted an empty DSN")
	}
}

func TestValidTemplateName(t *testing.T) {
	for _, name := range []string{
		"recon_eval_template",
		"recon_eval_template_test",
		"recon_eval_template-2",
	} {
		if !validTemplateName(name) && name != DefaultTemplate {
			t.Fatalf("validTemplateName(%q) = false", name)
		}
	}
	for _, name := range []string{
		"",
		"recon_eval",
		"recon_eval_template; DROP DATABASE x",
		"Recon_Eval_Template",
	} {
		if validTemplateName(name) {
			t.Fatalf("validTemplateName(%q) = true", name)
		}
	}
}
