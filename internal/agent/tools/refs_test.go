package tools_test

import (
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
)

func TestRefConstructors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		got  string
		err  error
		want string
	}{
		{
			name: "ledger txn",
			got:  mustRef(t, func() (string, error) { return tools.LedgerTxnRef(testTxnID) }),
			want: "ledger_txn:" + testTxnID,
		},
		{
			name: "external record",
			got:  mustRef(t, func() (string, error) { return tools.ExternalRef("psp_a", "record-1") }),
			want: "ext:psp_a:record-1",
		},
		{
			name: "external record with colon",
			got:  mustRef(t, func() (string, error) { return tools.ExternalRef("psp_a", "batch:record:1") }),
			want: "ext:psp_a:batch:record:1",
		},
		{
			name: "batch",
			got:  mustRef(t, func() (string, error) { return tools.BatchRef(testBatch) }),
			want: "batch:" + testBatch,
		},
		{
			name: "fx",
			got:  mustRef(t, func() (string, error) { return tools.FXRef("USD", "SEK", "2026-01-02") }),
			want: "fx:USD/SEK@2026-01-02",
		},
		{
			name: "fee schedule",
			got:  mustRef(t, func() (string, error) { return tools.FeeScheduleRef(testSched) }),
			want: "fee_schedule:" + testSched,
		},
		{
			name: "case",
			got:  mustRef(t, func() (string, error) { return tools.CaseRef(testCaseID) }),
			want: "case:" + testCaseID,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.err != nil {
				t.Fatalf("constructor error = %v", tt.err)
			}
			if tt.got != tt.want {
				t.Fatalf("ref = %q, want %q", tt.got, tt.want)
			}
			if err := tools.ValidateRef(tt.got); err != nil {
				t.Fatalf("ValidateRef(%q) = %v", tt.got, err)
			}
		})
	}
}

func mustRef(t *testing.T, call func() (string, error)) string {
	t.Helper()
	ref, err := call()
	if err != nil {
		t.Fatalf("constructor error = %v", err)
	}
	return ref
}

func TestRefConstructorsRejectInvalidValues(t *testing.T) {
	t.Parallel()

	type constructor func() (string, error)
	tests := []struct {
		name string
		new  constructor
	}{
		{name: "ledger empty", new: func() (string, error) { return tools.LedgerTxnRef("") }},
		{name: "ledger malformed", new: func() (string, error) { return tools.LedgerTxnRef("not-a-uuid") }},
		{name: "external empty source", new: func() (string, error) { return tools.ExternalRef("", "record") }},
		{name: "external malformed source", new: func() (string, error) { return tools.ExternalRef("psp a", "record") }},
		{name: "external empty record", new: func() (string, error) { return tools.ExternalRef("psp_a", "") }},
		{name: "external control char", new: func() (string, error) { return tools.ExternalRef("psp_a", "record\x00") }},
		{name: "batch malformed", new: func() (string, error) { return tools.BatchRef("nope") }},
		{name: "fx lower case", new: func() (string, error) { return tools.FXRef("usd", "SEK", "2026-01-02") }},
		{name: "fx same currency", new: func() (string, error) { return tools.FXRef("USD", "USD", "2026-01-02") }},
		{name: "fx bad date", new: func() (string, error) { return tools.FXRef("USD", "SEK", "2026-02-30") }},
		{name: "fx non-ISO currency", new: func() (string, error) { return tools.FXRef("USD", "ZZZ", "2026-01-02") }},
		{name: "fee schedule malformed", new: func() (string, error) { return tools.FeeScheduleRef("nope") }},
		{name: "case malformed", new: func() (string, error) { return tools.CaseRef("nope") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if ref, err := tt.new(); err == nil {
				t.Fatalf("constructor returned %q, want an error", ref)
			}
		})
	}
}

func TestValidateRef(t *testing.T) {
	t.Parallel()

	valid := []string{
		"ledger_txn:" + testTxnID,
		"ext:psp_a:record-1",
		"ext:bank_x:record:with:colons",
		"batch:" + testBatch,
		"fx:USD/SEK@2026-01-02",
		"fee_schedule:" + testSched,
		"case:" + testCaseID,
	}
	for _, ref := range valid {
		if err := tools.ValidateRef(ref); err != nil {
			t.Errorf("ValidateRef(%q) = %v, want nil", ref, err)
		}
	}

	invalid := []string{
		"",
		"nope:value",
		"ledger_txn:",
		"ledger_txn:not-a-uuid",
		"ext:",
		"ext:psp_a",
		"ext:psp_a:",
		"ext::record",
		"ext:psp a:record",
		"ext:psp_a:record\x00",
		"batch:not-a-uuid",
		"fx:USD@2026-01-02",
		"fx:USD/SEK",
		"fx:USD/SEK@2026-13-01",
		"fx:USD/USD@2026-01-02",
		"fx:usd/SEK@2026-01-02",
		"fee_schedule:not-a-uuid",
		"case:not-a-uuid",
		"case:" + strings.ToUpper(testCaseID) + "x",
	}
	for _, ref := range invalid {
		if err := tools.ValidateRef(ref); err == nil {
			t.Errorf("ValidateRef(%q) = nil, want an error", ref)
		}
	}
}
