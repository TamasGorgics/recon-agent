// covers: FIN-30, FIN-31, FIN-32, FIN-33, FIN-34

package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/audit"
)

func runAuditForTest(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := runAuditTo(context.Background(), args, &out)
	return out.String(), err
}

// TestAuditVerifyJSONExact pins spec 3.3: exactly four keys, findings sorted
// by (seq, kind), and an empty array when intact.
func TestAuditVerifyJSONExact(t *testing.T) {
	report := audit.Report{
		FromSeq: 3,
		HeadSeq: 9,
		// Checked is the surviving-row count; the CLI renders the head span.
		Checked: 5,
		Problems: []audit.Problem{
			{Seq: 7, Kind: audit.ProblemHashMismatch, Detail: "row hash mismatch"},
			{Seq: 3, Kind: audit.ProblemGap, Detail: "expected seq 3"},
			{Seq: 7, Kind: audit.ProblemBrokenLink, Detail: "prev hash mismatch"},
		},
	}

	var out bytes.Buffer
	if err := writeAuditVerifyJSON(&out, newAuditVerifyResult(report)); err != nil {
		t.Fatalf("writeAuditVerifyJSON: %v", err)
	}
	want := `{"from":3,"head_seq":9,"checked":7,"findings":[` +
		`{"seq":3,"kind":"gap","detail":"expected seq 3"},` +
		`{"seq":7,"kind":"broken_link","detail":"prev hash mismatch"},` +
		`{"seq":7,"kind":"hash_mismatch","detail":"row hash mismatch"}]}` + "\n"
	if out.String() != want {
		t.Fatalf("JSON = %s\nwant %s", out.String(), want)
	}
}

func TestAuditVerifyJSONEmptyFindingsIsArray(t *testing.T) {
	cases := []struct {
		name   string
		report audit.Report
		want   string
	}{
		{
			name:   "empty chain",
			report: audit.Report{FromSeq: 1, HeadSeq: 0},
			want:   `{"from":1,"head_seq":0,"checked":0,"findings":[]}` + "\n",
		},
		{
			name:   "head plus one",
			report: audit.Report{FromSeq: 5, HeadSeq: 4},
			want:   `{"from":5,"head_seq":4,"checked":0,"findings":[]}` + "\n",
		},
		{
			name:   "intact chain",
			report: audit.Report{FromSeq: 1, HeadSeq: 4, Checked: 4},
			want:   `{"from":1,"head_seq":4,"checked":4,"findings":[]}` + "\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := writeAuditVerifyJSON(&out, newAuditVerifyResult(tc.report)); err != nil {
				t.Fatalf("writeAuditVerifyJSON: %v", err)
			}
			if out.String() != tc.want {
				t.Fatalf("JSON = %s, want %s", out.String(), tc.want)
			}
			if strings.Contains(out.String(), "null") {
				t.Fatalf("JSON contains null: %s", out.String())
			}
		})
	}
}

// TestAuditVerifyCheckedUsesHeadSpan pins the spec 3.3 definition: checked is
// max(0, head_seq-from+1), not the number of rows that survived tampering.
func TestAuditVerifyCheckedUsesHeadSpan(t *testing.T) {
	report := audit.Report{FromSeq: 1, HeadSeq: 4, Checked: 3}
	if got := auditVerifyChecked(report); got != 4 {
		t.Fatalf("auditVerifyChecked(%+v) = %d, want 4", report, got)
	}
	if got := auditVerifyChecked(audit.Report{FromSeq: 5, HeadSeq: 4}); got != 0 {
		t.Fatalf("auditVerifyChecked(head+1) = %d, want 0", got)
	}
}

func TestAuditVerifyHumanOutput(t *testing.T) {
	intact := newAuditVerifyResult(audit.Report{FromSeq: 1, HeadSeq: 3, Checked: 3})
	var out bytes.Buffer
	if err := writeAuditVerifyHuman(&out, intact); err != nil {
		t.Fatalf("writeAuditVerifyHuman: %v", err)
	}
	if want := "audit verify: OK from=1 head=3 checked=3 findings=0\n"; out.String() != want {
		t.Fatalf("intact human output = %q, want %q", out.String(), want)
	}

	withFindings := newAuditVerifyResult(audit.Report{
		FromSeq: 1,
		HeadSeq: 9,
		Checked: 9,
		Problems: []audit.Problem{
			{Seq: 7, Kind: audit.ProblemHashMismatch, Detail: "row hash mismatch"},
			{Seq: 3, Kind: audit.ProblemGap, Detail: "expected seq 3"},
		},
	})
	out.Reset()
	if err := writeAuditVerifyHuman(&out, withFindings); err != nil {
		t.Fatalf("writeAuditVerifyHuman: %v", err)
	}
	want := "audit verify: FINDINGS from=1 head=9 checked=9 findings=2\n" +
		"  seq=3 kind=gap detail=expected seq 3\n" +
		"  seq=7 kind=hash_mismatch detail=row hash mismatch\n"
	if out.String() != want {
		t.Fatalf("finding human output = %q, want %q", out.String(), want)
	}
}

func TestAuditHelp(t *testing.T) {
	out, err := runAuditForTest(t, "verify", "--help")
	if err != nil {
		t.Fatalf("audit verify --help: %v", err)
	}
	for _, want := range []string{
		"usage: recon audit verify",
		"--from <seq>",
		"--json",
		"exit: 0 intact, 1 findings, 2 usage/config/connection/query error",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("help output does not contain %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "flag: help requested") {
		t.Errorf("help output leaks the flag package error:\n%s", out)
	}

	out, err = runAuditForTest(t, "help")
	if err != nil {
		t.Fatalf("audit help: %v", err)
	}
	for _, want := range []string{"usage: recon audit", "Commands:", "verify"} {
		if !strings.Contains(out, want) {
			t.Errorf("group help does not contain %q:\n%s", want, out)
		}
	}
}

func TestAuditUsageErrorsAreExitTwo(t *testing.T) {
	t.Setenv("CONFIG_FILE", "")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("DATABASE_APP_URL", "")

	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing command", args: nil,
			want: "missing command"},
		{name: "unknown command", args: []string{"nope"},
			want: "unknown audit command"},
		{name: "unknown flag", args: []string{"verify", "--nope"},
			want: "flag provided but not defined"},
		{name: "non integer from", args: []string{"verify", "--from", "abc"},
			want: `invalid value "abc"`},
		{name: "unexpected argument", args: []string{"verify", "--from", "1", "extra"},
			want: "unexpected argument"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runAuditForTest(t, tc.args...)
			if err == nil {
				t.Fatal("runAuditTo() = nil, want an error")
			}
			if code := exitCode(err); code != 2 {
				t.Fatalf("exit code = %d, want 2 (%v)", code, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.want)
			}
			if out != "" {
				t.Fatalf("usage error wrote to stdout: %q", out)
			}
		})
	}
}

func TestAuditVerifyRequiresAppURL(t *testing.T) {
	t.Setenv("CONFIG_FILE", "")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("DATABASE_APP_URL", "")

	out, err := runAuditForTest(t, "verify")
	if err == nil {
		t.Fatal("audit verify without database.app_url = nil, want an error")
	}
	if code := exitCode(err); code != 2 {
		t.Fatalf("exit code = %d, want 2 (%v)", code, err)
	}
	if !strings.Contains(err.Error(), "database.app_url is required") {
		t.Fatalf("error = %q, want the missing app_url message", err)
	}
	if out != "" {
		t.Fatalf("missing app_url wrote to stdout: %q", out)
	}
}

func TestAuditVerifyConfigLoadErrorIsExitTwo(t *testing.T) {
	t.Setenv("CONFIG_FILE", filepath.Join(t.TempDir(), "missing.yaml"))
	t.Setenv("DATABASE_APP_URL", "")

	out, err := runAuditForTest(t, "verify")
	if err == nil {
		t.Fatal("audit verify with an unreadable config = nil, want an error")
	}
	if code := exitCode(err); code != 2 {
		t.Fatalf("exit code = %d, want 2 (%v)", code, err)
	}
	if !strings.Contains(err.Error(), "audit verify") {
		t.Fatalf("error = %q, want it to name the command", err)
	}
	if out != "" {
		t.Fatalf("config error wrote to stdout: %q", out)
	}
}
