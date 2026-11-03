package testdb_test

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

// newDatabases returns the databases created during the test run that belong
// to this test process. Clone names embed the process id, so databases created
// concurrently by other package test binaries share the recon_test_ namespace
// but not this process's prefix: they are not orphans of this process. A clone
// leaked by this process is still reported. The shared template is never a
// per-test clone and is excluded explicitly.
func newDatabases(before, after []string, prefix string) []string {
	seen := make(map[string]struct{}, len(before))
	for _, name := range before {
		seen[name] = struct{}{}
	}
	var added []string
	for _, name := range after {
		if name == testdb.DefaultTemplate || !strings.HasPrefix(name, prefix) {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		added = append(added, name)
	}
	return added
}

func TestDatabasePrefixIsProcessScoped(t *testing.T) {
	want := fmt.Sprintf("recon_test_%d_", os.Getpid())
	if got := testdb.DatabasePrefix(); got != want {
		t.Fatalf("DatabasePrefix() = %q, want %q", got, want)
	}
	if testdb.DatabasePrefix() == testdb.DefaultTemplate {
		t.Fatalf("DatabasePrefix() collides with the template name %q", testdb.DefaultTemplate)
	}
}

// TestNewDatabasesScopedToProcess is the deterministic guard for F-1. It runs
// without a database: a foreign process's clone name must not be reported as
// this process's orphan, while a clone leaked by this process still must be.
func TestNewDatabasesScopedToProcess(t *testing.T) {
	prefix := testdb.DatabasePrefix()
	foreign := fmt.Sprintf("recon_test_%d_1_foreign", os.Getpid()+1)
	lookalike := fmt.Sprintf("recon_test_%d1_1_lookalike", os.Getpid())
	leaked := fmt.Sprintf("%s1_1_leaked", prefix)
	baseline := fmt.Sprintf("%s2_1_baseline", prefix)

	tests := []struct {
		name   string
		before []string
		after  []string
		want   []string
	}{
		{
			name:   "foreign process clones are not orphans",
			before: []string{testdb.DefaultTemplate},
			after:  []string{testdb.DefaultTemplate, foreign, lookalike},
		},
		{
			name:   "own leaked clone is an orphan",
			before: []string{testdb.DefaultTemplate},
			after:  []string{testdb.DefaultTemplate, foreign, leaked},
			want:   []string{leaked},
		},
		{
			name:   "own clone present at baseline is not new",
			before: []string{testdb.DefaultTemplate, baseline},
			after:  []string{testdb.DefaultTemplate, baseline, foreign},
		},
		{
			name:  "template is excluded",
			after: []string{testdb.DefaultTemplate},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := newDatabases(tc.before, tc.after, prefix); !slices.Equal(got, tc.want) {
				t.Fatalf("newDatabases() = %v, want %v", got, tc.want)
			}
		})
	}
}
