//go:build integration

package tieout_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/tieout"
)

type recordingTieoutObserver struct {
	computed []tieout.Result
	failures []string
}

func (o *recordingTieoutObserver) TieoutComputed(result tieout.Result) {
	o.computed = append(o.computed, result)
}

func (o *recordingTieoutObserver) TieoutFailed(code string) {
	o.failures = append(o.failures, code)
}

// TestComputeObserverRecordsSourceAndCurrency proves a successful computation
// reports the scope source and currency once and the CLI JSON shape does not
// gain a source field.
func TestComputeObserverRecordsSourceAndCurrency(t *testing.T) {
	f := newFixture(t, "psp_a")
	f.addStatement(t, "2026-06-10", 0, 0)
	observer := &recordingTieoutObserver{}

	result, err := tieout.Compute(context.Background(), f.db.Pool,
		f.scopeID, mustDate(t, "2026-06-10"), observer)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if result.Source != "psp_a" {
		t.Fatalf("result source = %q, want psp_a", result.Source)
	}
	if len(observer.failures) != 0 {
		t.Fatalf("successful computation emitted failures: %v", observer.failures)
	}
	if len(observer.computed) != 1 {
		t.Fatalf("computed observations = %d, want 1", len(observer.computed))
	}
	observed := observer.computed[0]
	if observed.Source != "psp_a" || observed.Currency != "USD" {
		t.Fatalf("observation source/currency = %q/%q, want psp_a/USD",
			observed.Source, observed.Currency)
	}
	if !observed.Unexplained.IsZero() {
		t.Fatalf("observed unexplained = %d, want 0", observed.Unexplained.AmountMinor())
	}

	encoded, err := result.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	if strings.Contains(string(encoded), `"source"`) {
		t.Fatalf("CLI JSON gained a source field: %s", encoded)
	}
}

// TestComputeObserverReportsBoundedFailures proves a failed computation emits
// only a bounded code and never a gauge value.
func TestComputeObserverReportsBoundedFailures(t *testing.T) {
	f := newFixture(t, "psp_a")
	observer := &recordingTieoutObserver{}

	_, err := tieout.Compute(context.Background(), f.db.Pool,
		"not-a-uuid", mustDate(t, "2026-06-10"), observer)
	if !errors.Is(err, tieout.ErrInvalidRequest) {
		t.Fatalf("invalid scope error = %v, want ErrInvalidRequest", err)
	}

	_, err = tieout.Compute(context.Background(), f.db.Pool,
		f.scopeID, mustDate(t, "2026-06-10"), observer)
	if !errors.Is(err, tieout.ErrNoStatement) {
		t.Fatalf("missing statement error = %v, want ErrNoStatement", err)
	}

	if len(observer.computed) != 0 {
		t.Fatalf("failed computations emitted results: %+v", observer.computed)
	}
	want := []string{tieout.ErrorCodeInvalidRequest, tieout.ErrorCodeNotFound}
	if len(observer.failures) != len(want) {
		t.Fatalf("failures = %v, want %v", observer.failures, want)
	}
	for i, code := range want {
		if observer.failures[i] != code {
			t.Fatalf("failure[%d] = %q, want %q", i, observer.failures[i], code)
		}
	}
}
