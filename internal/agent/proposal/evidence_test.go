package proposal

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
	"github.com/TamasGorgics/recon-agent/internal/clock"
)

func newEvidenceRun(t *testing.T, refs []string) *tools.RunContext {
	t.Helper()
	registry := tools.NewRegistry()
	if err := registry.Register(stubReadTool{name: "read_refs", refs: refs}); err != nil {
		t.Fatalf("register stub: %v", err)
	}
	fake := clock.NewFake(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	run := tools.NewRunContext(unitRunID, unitCaseID, nil, fake)
	runner := tools.NewRunner(registry, &fakeRecorder{}, fake)
	if _, err := runner.Call(context.Background(), run, 1, "read_refs", []byte(`{}`)); err != nil {
		t.Fatalf("stub read: %v", err)
	}
	return run
}

func TestEvidenceValidation(t *testing.T) {
	t.Parallel()
	refs := []string{"case:" + unitCaseID, "ledger_txn:" + unitTxnID}
	tests := []struct {
		name      string
		cited     []string
		runRefs   []string
		dbRefs    []string
		wantCause error
		want      []string
	}{
		{
			name:    "in memory and database agree",
			cited:   []string{"ledger_txn:" + unitTxnID, "case:" + unitCaseID, "case:" + unitCaseID},
			runRefs: refs,
			dbRefs:  refs,
			want:    refs,
		},
		{
			name:      "malformed ref",
			cited:     []string{"not a ref"},
			runRefs:   nil,
			dbRefs:    nil,
			wantCause: ErrInvalidEvidence,
		},
		{
			name:      "empty evidence",
			cited:     []string{},
			wantCause: ErrInvalidEvidence,
		},
		{
			name:      "missing from in-memory precheck",
			cited:     refs,
			runRefs:   []string{"case:" + unitCaseID},
			dbRefs:    refs,
			wantCause: ErrEvidenceNotRetrieved,
		},
		{
			name:      "database is authoritative after restart",
			cited:     refs,
			runRefs:   refs,
			dbRefs:    []string{"case:" + unitCaseID},
			wantCause: ErrEvidenceNotRetrieved,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			run := newEvidenceRun(t, test.runRefs)
			got, err := validateEvidence(context.Background(), run, test.cited,
				&fakeEvidenceReader{refs: test.dbRefs})
			if test.wantCause != nil {
				if !errors.Is(err, test.wantCause) {
					t.Fatalf("validateEvidence() error = %v, want %v", err, test.wantCause)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateEvidence(): %v", err)
			}
			if len(got) != len(test.want) {
				t.Fatalf("refs = %v, want %v", got, test.want)
			}
			for i := range test.want {
				if got[i] != test.want[i] {
					t.Fatalf("refs = %v, want %v", got, test.want)
				}
			}
		})
	}
}
