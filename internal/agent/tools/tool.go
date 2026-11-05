package tools

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/clock"
)

// MaxResultBytes is the maximum serialized success envelope size. If a result
// exceeds it, the runner replaces data with an omission marker, clears refs and
// untrusted_fields, and sets truncated=true.
const MaxResultBytes = 32 * 1024

// Tool is one callable agent tool. InputSchema is a JSON Schema with an object
// at the top level and additionalProperties=false on every object.
type Tool interface {
	Name() string
	Description() string
	InputSchema() json.RawMessage
	Call(ctx context.Context, run *RunContext, args json.RawMessage) (Result, error)
}

// Result is what a tool handler returns. The runner normalizes Refs (sorted,
// unique and validated) and serializes Data. Any per-record "untrusted_fields"
// array inside Data is unioned into the envelope's untrusted_fields; tools
// should therefore mark each external record they include.
type Result struct {
	Data      any
	Refs      []string
	Truncated bool
}

// Queryer is the read-only database surface available to tool bodies. It is
// satisfied by *pgxpool.Pool and pgx.Tx.
type Queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Deps is the dependency bundle passed to RegisterPart1/RegisterPart2 by the
// agent loop. Agent is the recon_agent_ro query surface; it is never the app
// pool.
type Deps struct {
	Agent           Queryer
	Clock           clock.Clock
	UntrustedFields map[string][]string
}

// Recorder persists one attempted tool call. The production implementation is
// DBRecorder, backed by the recon_app pool.
type Recorder interface {
	RecordToolCall(ctx context.Context, call ToolCall) error
}

// ToolCall is the persisted form of one attempted tool call. ErrorCode is the
// structured envelope code and is empty on success. Args and Result are the
// canonical JSON values stored in JSONB columns.
type ToolCall struct {
	RunID      string
	Seq        int
	ToolName   string
	Args       json.RawMessage
	Result     json.RawMessage
	ResultHash string
	ErrorCode  string
	StartedAt  time.Time
	DurationMS int
}

// RunContext is the per-run state shared by tool calls. Agent is the read-only
// queryer; Clock is the injected time source. The ref collector is private so
// only the runner can add refs from data that was actually returned.
type RunContext struct {
	RunID  string
	CaseID string
	Agent  Queryer
	Clock  clock.Clock

	mu   sync.Mutex
	refs map[string]struct{}
}

// NewRunContext creates a per-run context with an empty evidence-ref collector.
func NewRunContext(runID, caseID string, agent Queryer, clk clock.Clock) *RunContext {
	return &RunContext{
		RunID:  runID,
		CaseID: caseID,
		Agent:  agent,
		Clock:  clk,
		refs:   make(map[string]struct{}),
	}
}

// RetrievedRefs returns a sorted, unique copy of every evidence reference
// collected for this run.
func (r *RunContext) RetrievedRefs() []string {
	if r == nil {
		return []string{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	refs := make([]string, 0, len(r.refs))
	for ref := range r.refs {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	return refs
}

// addRefs records references from the final included data.
func (r *RunContext) addRefs(refs []string) {
	if r == nil || len(refs) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.refs == nil {
		r.refs = make(map[string]struct{}, len(refs))
	}
	for _, ref := range refs {
		r.refs[ref] = struct{}{}
	}
}
