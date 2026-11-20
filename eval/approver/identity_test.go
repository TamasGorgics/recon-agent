package approver

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

type recordingExecer struct {
	calls int
	query string
	args  []any
	err   error
}

func (r *recordingExecer) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	r.calls++
	r.query = sql
	r.args = args
	return pgconn.CommandTag{}, r.err
}

func TestSyntheticIdentityConstants(t *testing.T) {
	if !canonicalUUIDPattern.MatchString(SyntheticApproverID) {
		t.Fatalf("SyntheticApproverID %q is not a canonical lowercase UUID", SyntheticApproverID)
	}
	if SyntheticSource != "e2e" {
		t.Fatalf("SyntheticSource = %q, want e2e", SyntheticSource)
	}
	if SyntheticApproverDisplayName == "" || SyntheticApproverRole != "approver" {
		t.Fatalf("synthetic identity = %q/%q, want non-empty/approver",
			SyntheticApproverDisplayName, SyntheticApproverRole)
	}
}

func TestValidateApproverID(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		wantErr bool
	}{
		{name: "synthetic id", id: SyntheticApproverID},
		{name: "other canonical uuid", id: testCaseID},
		{name: "empty", id: " ", wantErr: true},
		{name: "padded", id: " " + SyntheticApproverID, wantErr: true},
		{name: "agent actor", id: "agent:worker", wantErr: true},
		{name: "bare agent", id: "agent", wantErr: true},
		{name: "system actor", id: "system:approval", wantErr: true},
		{name: "uppercase uuid", id: strings.ToUpper(SyntheticApproverID), wantErr: true},
		{name: "malformed uuid", id: "not-a-uuid", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateApproverID(test.id)
			if test.wantErr && !errors.Is(err, ErrNoApprover) {
				t.Fatalf("validateApproverID(%q) = %v, want ErrNoApprover", test.id, err)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("validateApproverID(%q) = %v, want nil", test.id, err)
			}
		})
	}
}

func TestEnsureSyntheticApproverRejectsInvalidInputs(t *testing.T) {
	ctx := context.Background()
	if err := EnsureSyntheticApprover(ctx, nil, SyntheticApproverID); err == nil {
		t.Fatal("EnsureSyntheticApprover(nil execer) = nil, want error")
	}

	var typedNil *recordingExecer
	if err := EnsureSyntheticApprover(ctx, typedNil, SyntheticApproverID); err == nil {
		t.Fatal("EnsureSyntheticApprover(typed nil execer) = nil, want error")
	}

	execer := &recordingExecer{}
	if err := EnsureSyntheticApprover(ctx, execer, "not-a-uuid"); !errors.Is(err, ErrNoApprover) {
		t.Fatalf("EnsureSyntheticApprover(bad id) = %v, want ErrNoApprover", err)
	}
	if execer.calls != 0 {
		t.Fatalf("Exec calls = %d, want 0 for invalid input", execer.calls)
	}
}

func TestEnsureSyntheticApproverIssuesIdempotentInsert(t *testing.T) {
	execer := &recordingExecer{}
	if err := EnsureSyntheticApprover(t.Context(), execer, SyntheticApproverID); err != nil {
		t.Fatalf("EnsureSyntheticApprover: %v", err)
	}
	if execer.calls != 1 {
		t.Fatalf("Exec calls = %d, want 1", execer.calls)
	}
	if !strings.Contains(execer.query, "INSERT INTO approvers") ||
		!strings.Contains(execer.query, "ON CONFLICT (id) DO NOTHING") {
		t.Fatalf("seed query does not use the idempotent insert: %s", execer.query)
	}
	if len(execer.args) != 3 {
		t.Fatalf("seed args = %v, want id/display/role", execer.args)
	}
	if execer.args[0] != SyntheticApproverID ||
		execer.args[1] != SyntheticApproverDisplayName ||
		execer.args[2] != SyntheticApproverRole {
		t.Fatalf("seed args = %v, want %q/%q/%q",
			execer.args, SyntheticApproverID, SyntheticApproverDisplayName, SyntheticApproverRole)
	}
}

func TestEnsureSyntheticApproverSurfacesExecError(t *testing.T) {
	sentinel := errors.New("permission denied")
	execer := &recordingExecer{err: sentinel}
	err := EnsureSyntheticApprover(t.Context(), execer, SyntheticApproverID)
	if !errors.Is(err, sentinel) {
		t.Fatalf("EnsureSyntheticApprover error = %v, want the exec error", err)
	}
}
