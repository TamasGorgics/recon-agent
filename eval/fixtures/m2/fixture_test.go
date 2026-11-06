//go:build integration

package m2_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"

	"github.com/TamasGorgics/recon-agent/eval/fixtures/m2"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

func TestMain(m *testing.M) {
	ctx := context.Background()
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		fmt.Fprintln(os.Stderr, "m2 fixture: TEST_DATABASE_URL is required for integration tests")
		os.Exit(1)
	}
	if _, err := testdb.EnsureTemplate(ctx, testdb.Options{AdminDSN: adminDSN}); err != nil {
		fmt.Fprintf(os.Stderr, "m2 fixture: ensure template: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// TestM2Fixture pins determinism: two fresh clones load byte-identical
// projections, and the manifest carries the locked expected proposal types.
func TestM2Fixture(t *testing.T) {
	ctx := context.Background()

	first := testdb.New(t)
	if err := m2.Load(ctx, first.Pool); err != nil {
		t.Fatalf("load first fixture: %v", err)
	}
	second := testdb.New(t)
	if err := m2.Load(ctx, second.Pool); err != nil {
		t.Fatalf("load second fixture: %v", err)
	}

	a, err := m2.ReadSnapshot(ctx, first.Pool)
	if err != nil {
		t.Fatalf("snapshot first fixture: %v", err)
	}
	b, err := m2.ReadSnapshot(ctx, second.Pool)
	if err != nil {
		t.Fatalf("snapshot second fixture: %v", err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("fixture projections differ between fresh clones:\nfirst:  %#v\nsecond: %#v", a, b)
	}

	want := map[string]string{
		m2.FeeVarianceCaseID:     "adjustment",
		m2.ReferenceTypoCaseID:   "link",
		m2.DuplicateChargeCaseID: "duplicate",
	}
	scenarios := m2.Scenarios()
	if len(scenarios) != 3 {
		t.Fatalf("scenarios = %d, want 3", len(scenarios))
	}
	for _, scenario := range scenarios {
		if want[scenario.CaseID] != scenario.ExpectedType {
			t.Errorf("scenario %s expects %q, want %q",
				scenario.Mode, scenario.ExpectedType, want[scenario.CaseID])
		}
	}
	if len(a.Cases) != 3 {
		t.Fatalf("fixture cases = %d, want 3", len(a.Cases))
	}
	for _, c := range a.Cases {
		if c.Status != "open" || c.Version != 0 || c.Attempts != 0 {
			t.Errorf("case %s = status %s version %d attempts %d, want open/0/0",
				c.ID, c.Status, c.Version, c.Attempts)
		}
	}
}

// TestM2FixtureRefusesReload pins the fresh-clone requirement: ledger rows are
// immutable, so a second load into the same database is a loud error.
func TestM2FixtureRefusesReload(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	if err := m2.Load(ctx, d.Pool); err != nil {
		t.Fatalf("first load: %v", err)
	}
	if err := m2.Load(ctx, d.Pool); !errors.Is(err, m2.ErrAlreadyLoaded) {
		t.Fatalf("second load error = %v, want ErrAlreadyLoaded", err)
	}
}
