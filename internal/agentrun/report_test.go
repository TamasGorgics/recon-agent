package agentrun

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// testReportQueryer is a narrow reportQueryer fake. It returns the configured
// tool-call rows from Query and the configured assistant/proposal rows from
// QueryRow.
type testReportQueryer struct {
	toolSteps [][]any
	assistant string
	proposal  []any
	queries   []string
}

func (q *testReportQueryer) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	q.queries = append(q.queries, sql)
	return &testRows{rows: q.toolSteps}, nil
}

func (q *testReportQueryer) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	q.queries = append(q.queries, sql)
	switch {
	case strings.Contains(sql, "llm_calls"):
		return testRow{values: []any{q.assistant}}
	case strings.Contains(sql, "proposals"):
		if q.proposal == nil {
			return testRow{err: pgx.ErrNoRows}
		}
		return testRow{values: q.proposal}
	default:
		return testRow{err: pgx.ErrNoRows}
	}
}

type testRows struct {
	rows [][]any
	pos  int
}

func (r *testRows) Close()                                       {}
func (r *testRows) Err() error                                   { return nil }
func (r *testRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *testRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *testRows) RawValues() [][]byte                          { return nil }
func (r *testRows) Conn() *pgx.Conn                              { return nil }
func (r *testRows) TypeMap() *pgtype.Map                         { return nil }

func (r *testRows) Next() bool {
	if r.pos >= len(r.rows) {
		return false
	}
	r.pos++
	return true
}

func (r *testRows) Scan(dest ...any) error {
	if r.pos == 0 || r.pos > len(r.rows) {
		return fmt.Errorf("test rows: Scan called without Next")
	}
	return scanTestValues(dest, r.rows[r.pos-1])
}

func (r *testRows) Values() ([]any, error) { return nil, nil }

type testRow struct {
	values []any
	err    error
}

func (r testRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	return scanTestValues(dest, r.values)
}

func scanTestValues(dest []any, values []any) error {
	if len(dest) != len(values) {
		return fmt.Errorf("test scan: got %d destinations for %d values", len(dest), len(values))
	}
	for i, d := range dest {
		switch target := d.(type) {
		case *int:
			value, ok := values[i].(int)
			if !ok {
				return fmt.Errorf("test scan: column %d is %T, want int", i, values[i])
			}
			*target = value
		case *string:
			value, ok := values[i].(string)
			if !ok {
				return fmt.Errorf("test scan: column %d is %T, want string", i, values[i])
			}
			*target = value
		case *[]byte:
			value, ok := values[i].([]byte)
			if !ok {
				return fmt.Errorf("test scan: column %d is %T, want []byte", i, values[i])
			}
			*target = value
		case *bool:
			value, ok := values[i].(bool)
			if !ok {
				return fmt.Errorf("test scan: column %d is %T, want bool", i, values[i])
			}
			*target = value
		default:
			return fmt.Errorf("test scan: unsupported destination %T", d)
		}
	}
	return nil
}

func TestFillReportLoadsSafeToolStepFieldsOnly(t *testing.T) {
	q := &testReportQueryer{
		toolSteps: [][]any{
			{
				2, "get_ledger", "tool_error",
				[]byte(`["ledger_txn:223e4567-e89b-42d3-a456-426614174000","case:123e4567-e89b-42d3-a456-426614174000","case:123e4567-e89b-42d3-a456-426614174000"]`),
				true, 7,
			},
			{1, "get_case", "", []byte(`[]`), false, 3},
		},
		assistant: "sanitized\x00 assistant text",
		proposal:  []any{"323e4567-e89b-42d3-a456-426614174000", "link", "awaiting_approval", "rationale"},
	}
	report := Report{
		CaseID: "123e4567-e89b-42d3-a456-426614174000",
		RunID:  "223e4567-e89b-42d3-a456-426614174000",
	}
	if err := fillReport(context.Background(), q, &report); err != nil {
		t.Fatalf("fillReport: %v", err)
	}

	if len(report.ToolSteps) != 2 {
		t.Fatalf("tool steps = %d, want 2", len(report.ToolSteps))
	}
	first := report.ToolSteps[0]
	if first.Seq != 2 || first.Tool != "get_ledger" || first.ErrorCode != "tool_error" {
		t.Fatalf("first step = %+v", first)
	}
	if !first.Truncated {
		t.Fatal("first step truncated = false, want true")
	}
	if got, want := strings.Join(first.Refs, ","), "case:123e4567-e89b-42d3-a456-426614174000,ledger_txn:223e4567-e89b-42d3-a456-426614174000"; got != want {
		t.Fatalf("first step refs = %q, want sorted unique %q", got, want)
	}
	if report.Assistant != "sanitized assistant text" {
		t.Fatalf("assistant = %q, want sanitized text", report.Assistant)
	}
	if report.ProposalID == "" || report.ProposalType != "link" {
		t.Fatalf("proposal = %q/%q", report.ProposalID, report.ProposalType)
	}

	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	text := string(encoded)
	for _, key := range []string{`"args"`, `"result"`, `"raw"`, `"memo"`} {
		if strings.Contains(text, key) {
			t.Fatalf("report JSON contains raw-data key %s: %s", key, text)
		}
	}

	toolQuery := q.queries[0]
	if strings.Contains(toolQuery, "args") {
		t.Fatalf("tool transcript query reads args: %s", toolQuery)
	}
	for _, needle := range []string{`result->'refs'`, `result->>'truncated'`} {
		if !strings.Contains(toolQuery, needle) {
			t.Fatalf("tool transcript query missing %q: %s", needle, toolQuery)
		}
	}
	for rest := toolQuery; ; {
		index := strings.Index(rest, "result")
		if index < 0 {
			break
		}
		if !strings.HasPrefix(rest[index:], "result->") {
			t.Fatalf("tool transcript query reads raw result: %s", toolQuery)
		}
		rest = rest[index+len("result"):]
	}
}
