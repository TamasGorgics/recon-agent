package demo

// M5-05 synthetic quality layer. This file writes deterministic eval rows
// (eval_runs / eval_case_results) plus the production-side approvals, apply
// attempts, and escalation history that feed the v_eval_* views. The layer is
// deliberately a strict superset of the M5-04 ops demo: RunQuality runs the M1
// pipeline and the ops layer first when they are absent, then adds the quality
// rows in one transaction.
//
// Everything here is synthetic dashboard evidence. It uses no network and no
// LLM, and it never claims to be a production eval run.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/db"
)

const (
	qualityIDNamespace    = "recon-demo-quality"
	qualityKeyPrefix      = "demo-quality:"
	qualityDatasetVersion = "demo-quality-v1"
	qualityChannel        = "demo-quality"
	qualityBanner         = "SYNTHETIC DEMO DATA — NOT PRODUCTION EVIDENCE"
)

// QualityModelA and the other identifiers are the stable synthetic dimensions
// reused by the eval rows and the production-side agent runs so the override
// and apply-failure panels join on real model/prompt values.
const (
	QualityModelA   = "demo-model-a"
	QualityModelB   = "demo-model-b"
	QualityPromptV1 = "demo-prompt-v1"
	QualityPromptV2 = "demo-prompt-v2"
)

// ErrQualityDataPresent is returned when the quality layer (or any eval row)
// already exists and the caller did not reset the scratch database first.
var ErrQualityDataPresent = errors.New(
	"demo quality: eval_runs already contains rows or demo-quality rows exist; " +
		"rerun with --reset on a scratch database")

// ErrQualityUntaggedData is returned when the database contains rows that are
// not provably part of the M1/ops/quality demo layers. Resetting would destroy
// non-demo evidence, so the caller must abort instead of wiping a shared
// database.
var ErrQualityUntaggedData = errors.New(
	"demo quality: database contains non-demo rows in tables the reset destroys; refusing to reset a shared database")

// QualitySummary is the deterministic `recon demo quality` result. It contains
// counts only, never UUIDs, so two runs with the same seed and clock print
// identical text.
type QualitySummary struct {
	Seed   int64
	Anchor string

	// Layers describes whether this run also seeded M1 + ops or only added the
	// quality layer on top of an existing demo ops seed.
	Layers string

	M1  *Summary
	Ops *OpsSummary

	EvalRuns       int
	EvalCaseResult int
	EvalDays       int
	Models         []string
	Prompts        []string
	Splits         map[string]int
	Suites         map[string]int

	Approvers       int
	AgentRuns       int
	Proposals       int
	Approvals       int
	Approved        int
	Rejected        int
	Applied         int
	ApplyFailed     int
	Cases           int
	Escalations     int
	EscalationKnown map[string]int
}

// Write prints the quality layer summary (counts only, no UUIDs).
func (s QualitySummary) Write(out io.Writer) error {
	if s.Ops != nil {
		if err := s.Ops.Write(out); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintf(out, "M1/ops demo layer: already present (not reseeded)\n"); err != nil {
			return err
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "M5-05 quality demo summary: seed=%d anchor=%s layers=%s\n",
		s.Seed, s.Anchor, s.Layers)
	fmt.Fprintf(&b, "  eval_runs=%d eval_case_results=%d days=%d models=%s prompts=%s\n",
		s.EvalRuns, s.EvalCaseResult, s.EvalDays,
		strings.Join(s.Models, ","), strings.Join(s.Prompts, ","))
	fmt.Fprintf(&b, "  splits: dev=%d test=%d injection=%d suites: agent=%d e2e=%d\n",
		s.Splits["dev"], s.Splits["test"], s.Splits["injection"],
		s.Suites["agent"], s.Suites["e2e"])
	fmt.Fprintf(&b, "  approvers=%d agent_runs=%d proposals=%d approvals=%d approved=%d rejected=%d\n",
		s.Approvers, s.AgentRuns, s.Proposals, s.Approvals, s.Approved, s.Rejected)
	fmt.Fprintf(&b, "  applied=%d apply_failed=%d cases=%d escalations=%d\n",
		s.Applied, s.ApplyFailed, s.Cases, s.Escalations)
	fmt.Fprintf(&b, "  escalation_reasons: policy=%d low_confidence=%d unknown=%d\n",
		s.EscalationKnown["policy"], s.EscalationKnown["low_confidence"], s.EscalationKnown["unknown"])

	_, err := io.WriteString(out, b.String())
	return err
}

// QualityBanner is the synthetic-data boundary printed at the start and end of
// `recon demo quality`.
func QualityBanner() string { return qualityBanner }

// CheckQualityResetAllowed is the read-only preflight for a destructive
// `recon demo quality --reset`. It inspects every table in the schemas the
// reset drops and aborts with *UntaggedDataError if any row is not demo-owned
// (M1 demo dataset, demo-ops, or demo-quality markers/IDs) or reproducible
// migration seed data. The authoritative check runs again inside the reset
// transaction after ACCESS EXCLUSIVE locks are taken; this preflight only
// produces an early, friendlier failure.
//
// The reset itself is the existing dev-only full schema reset. A tagged-only
// delete cannot be used for the quality layer because audit_log is append-only:
// escalation history from a previous seed cannot be removed, so repeated resets
// would double the escalation-reason panel. The full reset rebuilds the hash
// chain from the M1 pipeline and keeps the fixture deterministic.
func CheckQualityResetAllowed(ctx context.Context, dsn string) error {
	if strings.TrimSpace(dsn) == "" {
		return errors.New("demo quality: database URL is required")
	}
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("demo quality: open database: %w", err)
	}
	defer func() { _ = sqlDB.Close() }()
	if err := sqlDB.PingContext(ctx); err != nil {
		return fmt.Errorf("demo quality: connect database: %w", err)
	}
	return checkResetAllowed(ctx, sqlDB)
}

// RunQuality runs the M1 demo and the M5-04 ops layer when they are absent,
// then writes the deterministic M5-05 quality layer — all inside one READ
// COMMITTED transaction that takes EXCLUSIVE locks on every seed target table
// before any presence check.
//
// It fails closed before any write when eval_runs already has rows or any
// demo-quality row already exists. A writer that commits before the seed takes
// the locks is seen by the in-transaction checks (the seed aborts with no
// writes); a writer that starts after the locks blocks until the seed commits,
// so it can neither contaminate the seed nor be adopted as demo data. A
// failure at any layer rolls the one transaction back exactly; there is no
// compensating reset and no raw fallback, so untagged data committed by other
// sessions is never destroyed. The documented recovery path for a bad quality
// seed is the dev-only `recon demo quality --reset`, which rebuilds the scratch
// database.
func RunQuality(ctx context.Context, opts Options) (QualitySummary, error) {
	opts = opts.withDefaults()
	if strings.TrimSpace(opts.DBURL) == "" {
		return QualitySummary{}, errors.New("demo quality: database URL is required")
	}

	inputs, cleanup, err := prepareSeed(opts)
	if err != nil {
		return QualitySummary{}, err
	}
	defer cleanup()

	pool, err := db.NewPool(ctx, opts.DBURL)
	if err != nil {
		return QualitySummary{}, fmt.Errorf("demo quality: open database: %w", err)
	}
	defer pool.Close()

	// The reset preflight opens its own *sql.DB connection, so it must run
	// before the seed transaction takes EXCLUSIVE table locks: reading a
	// locked table through a second connection would block behind the lock.
	// These probes only decide whether the preflight is needed. The
	// authoritative fail-closed checks run inside the transaction, under the
	// locks.
	qualityPresent, err := qualityDataPresent(ctx, pool)
	if err != nil {
		return QualitySummary{}, fmt.Errorf("demo quality: check existing quality data: %w", err)
	}
	if !qualityPresent {
		opsPresent, err := opsDataPresent(ctx, pool)
		if err != nil {
			return QualitySummary{}, fmt.Errorf("demo quality: check existing ops data: %w", err)
		}
		if !opsPresent {
			// Seeding M1+ops is only safe on a database that already passed
			// the all-tables demo guard: anything else could be untagged
			// evidence. The preflight is read-only; the transaction rolls back
			// exactly on failure, so no compensating reset is ever needed.
			if err := CheckQualityResetAllowed(ctx, opts.DBURL); err != nil {
				return QualitySummary{}, err
			}
		}
	}

	var summary QualitySummary
	if err := db.WithTx(ctx, pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			if err := lockDemoSeed(ctx, tx); err != nil {
				return err
			}
			if err := lockSeedTables(ctx, tx); err != nil {
				return err
			}
			present, err := qualityDataPresent(ctx, tx)
			if err != nil {
				return fmt.Errorf("demo quality: check existing quality data: %w", err)
			}
			if present {
				return ErrQualityDataPresent
			}

			// M1 + ops are the lower layers of the superset. If a previous
			// `recon demo ops` (or `recon demo quality`) already seeded them,
			// keep those rows and add only the quality layer. If they are
			// absent, seed both in this same transaction. The probe above
			// already ran the reset preflight when this was the likely path;
			// if a concurrent guarded reset removed the rows in between, the
			// database was rebuilt to migration seed state, which the
			// preflight allows.
			opsPresent, err := opsDataPresent(ctx, tx)
			if err != nil {
				return fmt.Errorf("demo quality: check existing ops data: %w", err)
			}

			anchor := opts.Clock.Now().UTC().Truncate(time.Microsecond)
			plan := buildQualityPlan(opts.Seed, anchor)

			batchID, err := newDemoBatchID()
			if err != nil {
				return err
			}
			if !opsPresent {
				opsSummary, err := runOpsTx(ctx, opts, tx, inputs)
				if err != nil {
					return err
				}
				if err := insertQualityPlan(ctx, tx, plan, batchID); err != nil {
					return err
				}
				summary = plan.summary(&opsSummary)
				return nil
			}

			if err := insertQualityPlan(ctx, tx, plan, batchID); err != nil {
				return err
			}
			summary = plan.summary(nil)
			return nil
		}); err != nil {
		return QualitySummary{}, err
	}
	return summary, nil
}

// qualityDataPresent detects both the eval tables and the tagged production
// rows. Any eval_runs row is enough to fail closed, because seeding over an
// existing eval dataset would corrupt demo evidence.
func qualityDataPresent(ctx context.Context, q demoQuerier) (bool, error) {
	return qualityDataPresentQuerier(ctx, q)
}

type qualityRowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func qualityDataPresentQuerier(ctx context.Context, q qualityRowQuerier) (bool, error) {
	var present bool
	err := q.QueryRow(ctx,
		`SELECT EXISTS (
		    SELECT 1 FROM eval_runs
		    UNION ALL
		    SELECT 1 FROM proposals WHERE idempotency_key LIKE $1
		    UNION ALL
		    SELECT 1 FROM agent_runs WHERE trace_id LIKE $1
		    UNION ALL
		    SELECT 1 FROM approvers WHERE api_key_id LIKE $1
		    UNION ALL
		    SELECT 1 FROM approvals WHERE channel = $2
		)`, qualityKeyPrefix+"%", qualityChannel).Scan(&present)
	if err != nil {
		return false, err
	}
	return present, nil
}

// qualityEvalCase is one fixed literal case result. The values are a table,
// not a computation: there is no RNG and no time-derived jitter.
type qualityEvalCase struct {
	Key          string
	ID           string
	ScenarioID   string
	FailureMode  string
	Correct      bool
	FalseFix     bool
	Escalated    bool
	TierExpected *string
	TierActual   *string
	Confidence   string
	CostUSD      string
	LatencyMS    int
	Steps        int
	Detail       map[string]any
}

type qualityEvalRun struct {
	Key           string
	ID            string
	Model         string
	PromptVersion string
	Suite         string
	Split         string
	StartedAt     time.Time
	EndedAt       *time.Time
	RepeatGroup   *string
	RepeatIndex   *int
	Config        map[string]any
	Cases         []qualityEvalCase
}

type qualityApprover struct {
	Key         string
	ID          string
	DisplayName string
	Role        string
	APIKeyID    string
}

type qualityCase struct {
	Key              string
	ID               string
	Priority         int
	ValueMinor       int64
	Currency         string
	Status           string
	EscalationReason *string
	CreatedAt        time.Time
	ResolvedAt       *time.Time
	ResolutionType   *string
}

type qualityAgentRun struct {
	Key       string
	ID        string
	CaseID    string
	Model     string
	Prompt    string
	StartedAt time.Time
	EndedAt   time.Time
	TokensIn  int64
	TokensOut int64
	CostUSD   string
	TraceID   string
	Outcome   string
}

type qualityProposal struct {
	Key            string
	ID             string
	CaseID         string
	RunID          string
	Type           string
	Status         string
	RiskTier       string
	Confidence     string
	CreatedAt      time.Time
	ExpiresAt      *time.Time
	IdempotencyKey string
	StateHash      string
}

type qualityApproval struct {
	Key        string
	ID         string
	ProposalID string
	ApproverID string
	Decision   string
	DecidedAt  time.Time
}

type qualityApplied struct {
	ProposalID    string
	AppliedAt     time.Time
	ExecutorRunID string
}

type qualityAuditEntry struct {
	Key        string
	ActorType  string
	ActorID    string
	Action     string
	EntityType string
	EntityID   string
	Before     any
	After      any
	Timestamp  time.Time
}

type qualityPlan struct {
	Seed    int64
	Anchor  time.Time
	Models  []string
	Prompts []string

	Approvers  []qualityApprover
	Cases      []qualityCase
	Runs       []qualityAgentRun
	Proposals  []qualityProposal
	Approvals  []qualityApproval
	Applied    []qualityApplied
	Audits     []qualityAuditEntry
	EvalRuns   []qualityEvalRun
	EvalResult int
}

// qualityCaseFixtures is the fixed literal result table. Runs take a prefix of
// this table (4-6 rows); the injection and exclusion runs use their own fixed
// tables below.
var qualityCaseFixtures = []struct {
	Correct     bool
	FalseFix    bool
	Escalated   bool
	CostUSD     string
	LatencyMS   int
	Steps       int
	FailureMode string
}{
	{true, false, false, "0.012500", 1200, 4, "amount_mismatch"},
	{true, false, false, "0.018000", 1450, 5, "missing_external"},
	{false, false, true, "0.025000", 2100, 7, "duplicate_charge"},
	{true, false, false, "0.009500", 980, 3, "fee_variance"},
	{false, true, false, "0.031000", 2600, 9, "fx_rounding"},
	{true, false, false, "0.014000", 1320, 4, "reference_typo"},
	{true, false, true, "0.022000", 1875, 8, "amount_mismatch"},
	{false, false, false, "0.017000", 1500, 5, "missing_external"},
}

// qualityInjectionFixtures makes the injection split visible in false-fix and
// cost/latency but not in accuracy. The 0.777777/0.888888 costs and 9001/9101
// latencies are deliberately distinctive so a test can prove the runs are
// included in v_eval_false_fix / v_eval_cost_latency.
var qualityInjectionFixtures = []struct {
	Correct     bool
	FalseFix    bool
	Escalated   bool
	CostUSD     string
	LatencyMS   int
	Steps       int
	FailureMode string
}{
	{false, true, false, "0.777777", 9001, 11, "injection_false_fix"},
	{false, true, true, "0.888888", 9101, 12, "injection_escalation"},
	{true, false, false, "0.111111", 1101, 3, "injection_safe"},
	{false, false, false, "0.222222", 2202, 4, "injection_safe"},
}

// qualityExclusionFixtures are the repeat-group, unfinished, and matcher rows.
// The 777.000000 cost and 77777 ms latency would be obvious if any view
// accidentally included them.
var qualityExclusionFixtures = []struct {
	Correct     bool
	FalseFix    bool
	Escalated   bool
	CostUSD     string
	LatencyMS   int
	Steps       int
	FailureMode string
}{
	{true, false, false, "777.000000", 77777, 77, "excluded_repeat"},
	{false, true, false, "777.000000", 77777, 77, "excluded_unfinished"},
	{true, false, true, "777.000000", 77777, 77, "excluded_matcher"},
	{false, false, false, "777.000000", 77777, 77, "excluded_matcher"},
}

// qualityRunSpec is one row of the fixed eval-run table.
type qualityRunSpec struct {
	Key       string
	DayOffset int
	Suite     string
	Split     string
	Model     string
	Prompt    string
	CaseCount int
	Kind      string // "regular", "injection", "exclusion"
	Repeat    bool
	Unended   bool
}

var qualityRunSpecs = []qualityRunSpec{
	{Key: "a-v1-dev", DayOffset: -6, Suite: "agent", Split: "dev", Model: QualityModelA, Prompt: QualityPromptV1, CaseCount: 6, Kind: "regular"},
	{Key: "a-v1-test", DayOffset: -4, Suite: "agent", Split: "test", Model: QualityModelA, Prompt: QualityPromptV1, CaseCount: 5, Kind: "regular"},
	{Key: "a-v2-dev", DayOffset: -5, Suite: "agent", Split: "dev", Model: QualityModelA, Prompt: QualityPromptV2, CaseCount: 6, Kind: "regular"},
	{Key: "a-v2-e2e", DayOffset: -3, Suite: "e2e", Split: "test", Model: QualityModelA, Prompt: QualityPromptV2, CaseCount: 5, Kind: "regular"},
	{Key: "b-v1-dev", DayOffset: -4, Suite: "agent", Split: "dev", Model: QualityModelB, Prompt: QualityPromptV1, CaseCount: 6, Kind: "regular"},
	{Key: "b-v1-e2e", DayOffset: -2, Suite: "e2e", Split: "test", Model: QualityModelB, Prompt: QualityPromptV1, CaseCount: 5, Kind: "regular"},
	{Key: "b-v2-dev", DayOffset: -1, Suite: "agent", Split: "dev", Model: QualityModelB, Prompt: QualityPromptV2, CaseCount: 6, Kind: "regular"},
	{Key: "b-v2-test", DayOffset: 0, Suite: "agent", Split: "test", Model: QualityModelB, Prompt: QualityPromptV2, CaseCount: 5, Kind: "regular"},
	{Key: "a-v1-injection", DayOffset: -2, Suite: "agent", Split: "injection", Model: QualityModelA, Prompt: QualityPromptV1, CaseCount: 4, Kind: "injection"},
	{Key: "b-v2-injection", DayOffset: 0, Suite: "e2e", Split: "injection", Model: QualityModelB, Prompt: QualityPromptV2, CaseCount: 4, Kind: "injection"},
	{Key: "excluded-repeat", DayOffset: -1, Suite: "agent", Split: "test", Model: QualityModelA, Prompt: QualityPromptV1, CaseCount: 4, Kind: "exclusion", Repeat: true},
	{Key: "excluded-unfinished", DayOffset: -1, Suite: "agent", Split: "test", Model: QualityModelB, Prompt: QualityPromptV2, CaseCount: 4, Kind: "exclusion", Unended: true},
	{Key: "excluded-matcher", DayOffset: -1, Suite: "matcher", Split: "test", Model: QualityModelA, Prompt: QualityPromptV1, CaseCount: 4, Kind: "exclusion"},
}

// buildQualityPlan is pure: the same seed and anchor always produce the same
// IDs, timestamps, case values, and audit payloads.
func buildQualityPlan(seed int64, now time.Time) qualityPlan {
	now = now.UTC().Truncate(time.Microsecond)
	anchor := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	at := func(dayOffset, hour, minute int) time.Time {
		return anchor.AddDate(0, 0, dayOffset).Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute)
	}
	ptrTime := func(t time.Time) *time.Time { return &t }
	ptrString := func(s string) *string { return &s }
	ptrInt := func(v int) *int { return &v }
	id := func(key string) string { return qualityUUID(seed, key) }

	plan := qualityPlan{
		Seed:    seed,
		Anchor:  anchor,
		Models:  []string{QualityModelA, QualityModelB},
		Prompts: []string{QualityPromptV1, QualityPromptV2},
	}

	plan.Approvers = []qualityApprover{
		{
			Key: "approver-1", ID: id("approver:1"),
			DisplayName: "Demo Quality Approver One", Role: "approver",
			APIKeyID: qualityKey(seed, "approver:1"),
		},
		{
			Key: "approver-2", ID: id("approver:2"),
			DisplayName: "Demo Quality Approver Two", Role: "approver",
			APIKeyID: qualityKey(seed, "approver:2"),
		},
	}

	// Production-side cases. Cases 1-4 are the escalated history, 5-10 are
	// resolved/open cases with applied or rejected proposals, and 11-12 host
	// the two apply_failed attempts.
	caseSpecs := []struct {
		Key           string
		Status        string
		Reason        *string
		CreatedAt     time.Time
		ResolvedAt    *time.Time
		Resolution    *string
		EscalatedAt   *time.Time
		EscalationKey string
	}{
		{Key: "case-1", Status: "escalated", Reason: ptrString("policy"), CreatedAt: at(-6, 6, 0), EscalatedAt: ptrTime(at(-6, 7, 0)), EscalationKey: "case-1"},
		{Key: "case-2", Status: "escalated", Reason: ptrString("low_confidence"), CreatedAt: at(-5, 6, 0), EscalatedAt: ptrTime(at(-5, 9, 0)), EscalationKey: "case-2"},
		{Key: "case-3", Status: "escalated", Reason: ptrString("unknown"), CreatedAt: at(-4, 6, 0), EscalatedAt: ptrTime(at(-4, 10, 0)), EscalationKey: "case-3"},
		{Key: "case-4", Status: "escalated", Reason: ptrString("policy"), CreatedAt: at(-3, 6, 0), EscalatedAt: ptrTime(at(-2, 11, 0)), EscalationKey: "case-4"},
		{Key: "case-5", Status: "resolved", CreatedAt: at(-6, 8, 0), ResolvedAt: ptrTime(at(-6, 11, 0)), Resolution: ptrString("human_approved")},
		{Key: "case-6", Status: "resolved", CreatedAt: at(-5, 8, 0), ResolvedAt: ptrTime(at(-5, 11, 0)), Resolution: ptrString("human_approved")},
		{Key: "case-7", Status: "resolved", CreatedAt: at(-4, 8, 0), ResolvedAt: ptrTime(at(-4, 11, 0)), Resolution: ptrString("human_approved")},
		{Key: "case-8", Status: "open", CreatedAt: at(-2, 8, 0)},
		{Key: "case-9", Status: "open", CreatedAt: at(-1, 8, 0)},
		{Key: "case-10", Status: "open", CreatedAt: at(0, 8, 0)},
		{Key: "case-11", Status: "open", CreatedAt: at(-2, 8, 30)},
		{Key: "case-12", Status: "open", CreatedAt: at(-4, 8, 30)},
	}
	caseIDs := make(map[string]string, len(caseSpecs))
	for i, spec := range caseSpecs {
		caseID := id("case:" + spec.Key)
		caseIDs[spec.Key] = caseID
		plan.Cases = append(plan.Cases, qualityCase{
			Key: spec.Key, ID: caseID, Priority: 1 + i%3,
			ValueMinor: int64(10_000 + i*7_500), Currency: "USD",
			Status: spec.Status, EscalationReason: spec.Reason,
			CreatedAt: spec.CreatedAt, ResolvedAt: spec.ResolvedAt,
			ResolutionType: spec.Resolution,
		})
		if spec.EscalatedAt != nil {
			after := map[string]any{"status": "escalated", "version": 1}
			if spec.Reason != nil && *spec.Reason != "unknown" {
				after["reason"] = *spec.Reason
				after["escalation_reason"] = *spec.Reason
			}
			plan.Audits = append(plan.Audits, qualityAuditEntry{
				Key: spec.EscalationKey, ActorType: "system", ActorID: "cases",
				Action: "case.transition", EntityType: "case", EntityID: caseID,
				Before:    map[string]any{"status": "open", "version": 0},
				After:     after,
				Timestamp: *spec.EscalatedAt,
			})
		}
	}

	// Production-side agent runs and proposals. Every proposal is terminal, so
	// the one-active-proposal-per-case index is never exercised.
	type proposalSpec struct {
		Key         string
		CaseKey     string
		Model       string
		Prompt      string
		Status      string
		CreatedAt   time.Time
		AppliedAt   *time.Time
		Approval    *qualityApproval
		FailedAt    *time.Time
		FailedCause string
	}
	approverIDs := map[string]string{
		"approver-1": plan.Approvers[0].ID,
		"approver-2": plan.Approvers[1].ID,
	}
	approvalSpec := func(proposalKey, approverKey, decision string, at time.Time) *qualityApproval {
		return &qualityApproval{
			Key: proposalKey, ID: id("approval:" + proposalKey),
			ProposalID: id("proposal:" + proposalKey),
			ApproverID: approverIDs[approverKey],
			Decision:   decision, DecidedAt: at,
		}
	}
	proposalSpecs := []proposalSpec{
		{Key: "proposal-1", CaseKey: "case-5", Model: QualityModelA, Prompt: QualityPromptV1, Status: "applied",
			CreatedAt: at(-6, 9, 0), AppliedAt: ptrTime(at(-6, 10, 0)),
			Approval: approvalSpec("proposal-1", "approver-1", "approved", at(-6, 9, 30))},
		{Key: "proposal-2", CaseKey: "case-6", Model: QualityModelB, Prompt: QualityPromptV2, Status: "applied",
			CreatedAt: at(-5, 9, 0), AppliedAt: ptrTime(at(-5, 10, 0)),
			Approval: approvalSpec("proposal-2", "approver-2", "approved", at(-5, 9, 30))},
		{Key: "proposal-3", CaseKey: "case-7", Model: QualityModelA, Prompt: QualityPromptV2, Status: "applied",
			CreatedAt: at(-4, 9, 0), AppliedAt: ptrTime(at(-4, 10, 0)),
			Approval: approvalSpec("proposal-3", "approver-1", "approved", at(-4, 9, 30))},
		{Key: "proposal-4", CaseKey: "case-8", Model: QualityModelB, Prompt: QualityPromptV1, Status: "applied",
			CreatedAt: at(-2, 9, 0), AppliedAt: ptrTime(at(-2, 10, 0))},
		{Key: "proposal-5", CaseKey: "case-3", Model: QualityModelA, Prompt: QualityPromptV1, Status: "rejected",
			CreatedAt: at(-4, 9, 30), Approval: approvalSpec("proposal-5", "approver-2", "rejected", at(-4, 11, 0))},
		{Key: "proposal-6", CaseKey: "case-4", Model: QualityModelB, Prompt: QualityPromptV2, Status: "rejected",
			CreatedAt: at(-3, 9, 0), Approval: approvalSpec("proposal-6", "approver-2", "rejected", at(-3, 12, 0))},
		{Key: "proposal-7", CaseKey: "case-9", Model: QualityModelA, Prompt: QualityPromptV2, Status: "applied",
			CreatedAt: at(-1, 9, 0), AppliedAt: ptrTime(at(-1, 10, 0))},
		{Key: "proposal-8", CaseKey: "case-10", Model: QualityModelB, Prompt: QualityPromptV1, Status: "applied",
			CreatedAt: at(0, 9, 0), AppliedAt: ptrTime(at(0, 10, 0))},
		{Key: "proposal-9", CaseKey: "case-11", Model: QualityModelA, Prompt: QualityPromptV1, Status: "apply_failed",
			CreatedAt: at(-2, 9, 15), FailedAt: ptrTime(at(-2, 10, 0)), FailedCause: "state_changed"},
		{Key: "proposal-10", CaseKey: "case-12", Model: QualityModelB, Prompt: QualityPromptV2, Status: "apply_failed",
			CreatedAt: at(-4, 9, 15), FailedAt: ptrTime(at(-3, 10, 0)), FailedCause: "break_resolved"},
	}
	for i, spec := range proposalSpecs {
		runID := id("run:" + spec.Key)
		proposalID := id("proposal:" + spec.Key)
		plan.Runs = append(plan.Runs, qualityAgentRun{
			Key: spec.Key, ID: runID, CaseID: caseIDs[spec.CaseKey],
			Model: spec.Model, Prompt: spec.Prompt,
			StartedAt: spec.CreatedAt.Add(-10 * time.Minute),
			EndedAt:   spec.CreatedAt.Add(-time.Minute),
			TokensIn:  int64(900 + i*173),
			TokensOut: int64(140 + i*37),
			CostUSD:   fmt.Sprintf("0.%06d", 11000+i*211),
			TraceID:   qualityKey(seed, "run:"+spec.Key),
			Outcome:   "proposal",
		})
		plan.Proposals = append(plan.Proposals, qualityProposal{
			Key: spec.Key, ID: proposalID, CaseID: caseIDs[spec.CaseKey], RunID: runID,
			Type: "link", Status: spec.Status, RiskTier: []string{"T0", "T1", "T2"}[i%3],
			Confidence:     fmt.Sprintf("0.%04d", 8000+i*137),
			CreatedAt:      spec.CreatedAt,
			ExpiresAt:      ptrTime(spec.CreatedAt.Add(48 * time.Hour)),
			IdempotencyKey: qualityKey(seed, "proposal:"+spec.Key),
			StateHash:      qualityHash(seed, "state:"+spec.Key),
		})
		if spec.Approval != nil {
			plan.Approvals = append(plan.Approvals, *spec.Approval)
		}
		if spec.AppliedAt != nil {
			plan.Applied = append(plan.Applied, qualityApplied{
				ProposalID:    proposalID,
				AppliedAt:     *spec.AppliedAt,
				ExecutorRunID: qualityKey(seed, "exec:"+spec.Key),
			})
		}
		if spec.FailedAt != nil {
			plan.Audits = append(plan.Audits, qualityAuditEntry{
				Key: "apply-failed:" + spec.Key, ActorType: "system", ActorID: "executor",
				Action: "proposal.transition", EntityType: "proposal", EntityID: proposalID,
				Before: map[string]any{"status": "approved", "version": 0},
				After: map[string]any{
					"status": "apply_failed", "version": 1, "reason": spec.FailedCause,
				},
				Timestamp: *spec.FailedAt,
			})
		}
	}

	// Fixed literal eval-run table. All timestamps are derived from the anchor
	// day only; the case values come from the literal tables above.
	for runIndex, spec := range qualityRunSpecs {
		runID := id("eval-run:" + spec.Key)
		startedAt := at(spec.DayOffset, 6+runIndex%3, runIndex*7%60)
		var endedAt *time.Time
		if !spec.Unended {
			endedAt = ptrTime(startedAt.Add(90 * time.Minute))
		}
		var repeatGroup *string
		var repeatIndex *int
		if spec.Repeat {
			group := id("repeat-group:" + spec.Key)
			repeatGroup = &group
			repeatIndex = ptrInt(0)
		}
		run := qualityEvalRun{
			Key: spec.Key, ID: runID, Model: spec.Model, PromptVersion: spec.Prompt,
			Suite: spec.Suite, Split: spec.Split, StartedAt: startedAt, EndedAt: endedAt,
			RepeatGroup: repeatGroup, RepeatIndex: repeatIndex,
			Config: map[string]any{
				"demo_quality": true, "seed": seed, "run_key": spec.Key,
			},
		}
		fixtures := qualityCaseFixtures
		switch spec.Kind {
		case "injection":
			fixtures = qualityInjectionFixtures
		case "exclusion":
			fixtures = qualityExclusionFixtures
		}
		for caseIndex := 0; caseIndex < spec.CaseCount; caseIndex++ {
			fixture := fixtures[caseIndex]
			scenarioID := fmt.Sprintf("%s%s:%d", qualityKeyPrefix, spec.Key, caseIndex)
			detail := map[string]any{
				"demo_quality": true, "seed": seed,
				"run_key": spec.Key, "scenario": scenarioID,
			}
			if spec.Suite == "e2e" {
				detail["e2e"] = map[string]any{"schema": "eval-e2e/v1"}
			}
			tier := []string{"T0", "T1", "T2"}[caseIndex%3]
			run.Cases = append(run.Cases, qualityEvalCase{
				Key:        fmt.Sprintf("%s-%d", spec.Key, caseIndex),
				ID:         qualityUUID(seed, fmt.Sprintf("eval-case:%s:%d", spec.Key, caseIndex)),
				ScenarioID: scenarioID, FailureMode: fixture.FailureMode,
				Correct: fixture.Correct, FalseFix: fixture.FalseFix,
				Escalated:    fixture.Escalated,
				TierExpected: ptrString(tier), TierActual: ptrString(tier),
				Confidence: fmt.Sprintf("0.%04d", 7000+caseIndex*311),
				CostUSD:    fixture.CostUSD, LatencyMS: fixture.LatencyMS,
				Steps: fixture.Steps, Detail: detail,
			})
			plan.EvalResult++
		}
		plan.EvalRuns = append(plan.EvalRuns, run)
	}

	return plan
}

func (p qualityPlan) summary(ops *OpsSummary) QualitySummary {
	s := QualitySummary{
		Seed:            p.Seed,
		Anchor:          p.Anchor.Format("2006-01-02"),
		Layers:          "M1+ops+quality",
		Ops:             ops,
		EvalRuns:        len(p.EvalRuns),
		EvalCaseResult:  p.EvalResult,
		Models:          append([]string(nil), p.Models...),
		Prompts:         append([]string(nil), p.Prompts...),
		Splits:          map[string]int{},
		Suites:          map[string]int{},
		Approvers:       len(p.Approvers),
		AgentRuns:       len(p.Runs),
		Proposals:       len(p.Proposals),
		Approvals:       len(p.Approvals),
		Applied:         len(p.Applied),
		Cases:           len(p.Cases),
		EscalationKnown: map[string]int{"policy": 0, "low_confidence": 0, "unknown": 0},
	}
	if ops == nil {
		s.Layers = "quality (M1+ops already present)"
	}
	days := map[string]struct{}{}
	for _, run := range p.EvalRuns {
		s.Splits[run.Split]++
		s.Suites[run.Suite]++
		days[run.StartedAt.Format("2006-01-02")] = struct{}{}
	}
	s.EvalDays = len(days)
	for _, approval := range p.Approvals {
		switch approval.Decision {
		case "approved":
			s.Approved++
		case "rejected":
			s.Rejected++
		}
	}
	for _, proposal := range p.Proposals {
		if proposal.Status == "apply_failed" {
			s.ApplyFailed++
		}
	}
	for _, entry := range p.Audits {
		if entry.Action != "case.transition" || entry.EntityType != "case" {
			continue
		}
		after, ok := entry.After.(map[string]any)
		if !ok || after["status"] != "escalated" {
			continue
		}
		s.Escalations++
		reason := "unknown"
		if value, ok := after["reason"].(string); ok && value != "" {
			reason = value
		}
		s.EscalationKnown[reason]++
	}
	return s
}

func insertQualityPlan(ctx context.Context, tx pgx.Tx, plan qualityPlan, batchID string) error {
	present, err := qualityDataPresentQuerier(ctx, tx)
	if err != nil {
		return fmt.Errorf("demo quality: re-check existing quality data: %w", err)
	}
	if present {
		return ErrQualityDataPresent
	}

	for _, approver := range plan.Approvers {
		if _, err := tx.Exec(ctx,
			`INSERT INTO approvers (id, display_name, role, api_key_id)
				 VALUES ($1::uuid, $2, $3, $4)`,
			approver.ID, approver.DisplayName, approver.Role, approver.APIKeyID); err != nil {
			return fmt.Errorf("demo quality: insert approver %s: %w", approver.Key, err)
		}
	}
	for _, c := range plan.Cases {
		if _, err := tx.Exec(ctx,
			`INSERT INTO cases
				     (id, priority, value_minor, currency, status, version, attempts,
				      escalation_reason, created_at, resolved_at, resolution_type)
				 VALUES ($1::uuid, $2, $3, $4, $5, 0, 0, $6, $7, $8, $9)`,
			c.ID, c.Priority, c.ValueMinor, c.Currency, c.Status,
			c.EscalationReason, c.CreatedAt, c.ResolvedAt, c.ResolutionType); err != nil {
			return fmt.Errorf("demo quality: insert case %s: %w", c.Key, err)
		}
	}
	for _, run := range plan.Runs {
		if _, err := tx.Exec(ctx,
			`INSERT INTO agent_runs
				     (id, case_id, model, prompt_version, started_at, ended_at,
				      tokens_in, tokens_out, cost_usd, trace_id, outcome)
				 VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8, $9::numeric, $10, $11)`,
			run.ID, run.CaseID, run.Model, run.Prompt, run.StartedAt, run.EndedAt,
			run.TokensIn, run.TokensOut, run.CostUSD, run.TraceID, run.Outcome); err != nil {
			return fmt.Errorf("demo quality: insert agent run %s: %w", run.Key, err)
		}
	}
	for _, proposal := range plan.Proposals {
		if _, err := tx.Exec(ctx,
			`INSERT INTO proposals
				     (id, case_id, agent_run_id, type, payload, rationale, evidence,
				      confidence, risk_tier, status, version, state_hash,
				      idempotency_key, created_at, expires_at)
				 VALUES ($1::uuid, $2::uuid, $3::uuid, $4, '{}'::jsonb, $5,
				         '[]'::jsonb, $6, $7, $8, 0, $9, $10, $11, $12)`,
			proposal.ID, proposal.CaseID, proposal.RunID, proposal.Type,
			"Demo quality synthetic proposal for the M5-05 dashboard.",
			proposal.Confidence, proposal.RiskTier, proposal.Status,
			proposal.StateHash, proposal.IdempotencyKey,
			proposal.CreatedAt, proposal.ExpiresAt); err != nil {
			return fmt.Errorf("demo quality: insert proposal %s: %w", proposal.Key, err)
		}
	}
	for _, approval := range plan.Approvals {
		if _, err := tx.Exec(ctx,
			`INSERT INTO approvals
				     (id, proposal_id, channel, approver_id, decision, decided_at, signature_valid)
				 VALUES ($1::uuid, $2::uuid, $3, $4::uuid, $5, $6, true)`,
			approval.ID, approval.ProposalID, qualityChannel, approval.ApproverID,
			approval.Decision, approval.DecidedAt); err != nil {
			return fmt.Errorf("demo quality: insert approval %s: %w", approval.Key, err)
		}
	}
	for _, applied := range plan.Applied {
		if _, err := tx.Exec(ctx,
			`INSERT INTO applied_proposals
				     (proposal_id, ledger_txn_id, applied_at, executor_run_id)
				 VALUES ($1::uuid, NULL, $2, $3)`,
			applied.ProposalID, applied.AppliedAt, applied.ExecutorRunID); err != nil {
			return fmt.Errorf("demo quality: insert applied proposal: %w", err)
		}
	}
	auditIDs := make([]string, 0, len(plan.Audits))
	auditSeqs := make([]string, 0, len(plan.Audits))
	for _, entry := range plan.Audits {
		actor, err := audit.ParseActor(entry.ActorType + ":" + entry.ActorID)
		if err != nil {
			return fmt.Errorf("demo quality: audit actor %s/%s: %w",
				entry.ActorType, entry.ActorID, err)
		}
		record, err := audit.Append(ctx, tx, audit.Entry{
			Actor: actor, Action: entry.Action,
			EntityType: entry.EntityType, EntityID: entry.EntityID,
			Before: entry.Before, After: entry.After,
			Timestamp: entry.Timestamp,
		})
		if err != nil {
			return fmt.Errorf("demo quality: append audit %s: %w", entry.Key, err)
		}
		auditIDs = append(auditIDs, record.ID)
		auditSeqs = append(auditSeqs, fmt.Sprintf("%d", record.Seq))
	}
	for _, run := range plan.EvalRuns {
		config, err := json.Marshal(run.Config)
		if err != nil {
			return fmt.Errorf("demo quality: encode eval run %s config: %w", run.Key, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO eval_runs
				     (id, suite, split, dataset_version, model, prompt_version, seed,
				      config, started_at, ended_at, repeat_group, repeat_index)
				 VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8::jsonb, $9, $10, $11::uuid, $12)`,
			run.ID, run.Suite, run.Split, qualityDatasetVersion, run.Model, run.PromptVersion,
			plan.Seed, string(config), run.StartedAt, run.EndedAt,
			run.RepeatGroup, run.RepeatIndex); err != nil {
			return fmt.Errorf("demo quality: insert eval run %s: %w", run.Key, err)
		}
		for _, result := range run.Cases {
			detail, err := json.Marshal(result.Detail)
			if err != nil {
				return fmt.Errorf("demo quality: encode eval case %s detail: %w", result.Key, err)
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO eval_case_results
					     (id, eval_run_id, scenario_id, failure_mode, correct, false_fix,
					      escalated, tier_expected, tier_actual, confidence, cost_usd,
					      latency_ms, steps, detail)
					 VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8, $9, $10,
					         $11::numeric, $12, $13, $14::jsonb)`,
				result.ID, run.ID, result.ScenarioID, result.FailureMode,
				result.Correct, result.FalseFix, result.Escalated,
				result.TierExpected, result.TierActual, result.Confidence,
				result.CostUSD, result.LatencyMS, result.Steps, string(detail)); err != nil {
				return fmt.Errorf("demo quality: insert eval case %s: %w", result.Key, err)
			}
		}
	}
	return markQualityPlanRows(ctx, tx, plan, batchID, auditIDs, auditSeqs)
}

// markQualityPlanRows records the quality rows inside the same outer seed
// transaction, so the reset guard can prove every row is demo-owned even if the
// process dies immediately after commit.
func markQualityPlanRows(ctx context.Context, tx pgx.Tx, plan qualityPlan, batchID string, auditIDs, auditSeqs []string) error {
	approverIDs := make([]string, 0, len(plan.Approvers))
	for _, approver := range plan.Approvers {
		approverIDs = append(approverIDs, approver.ID)
	}
	caseIDs := make([]string, 0, len(plan.Cases))
	for _, c := range plan.Cases {
		caseIDs = append(caseIDs, c.ID)
	}
	runIDs := make([]string, 0, len(plan.Runs))
	for _, run := range plan.Runs {
		runIDs = append(runIDs, run.ID)
	}
	proposalIDs := make([]string, 0, len(plan.Proposals))
	for _, proposal := range plan.Proposals {
		proposalIDs = append(proposalIDs, proposal.ID)
	}
	approvalIDs := make([]string, 0, len(plan.Approvals))
	for _, approval := range plan.Approvals {
		approvalIDs = append(approvalIDs, approval.ID)
	}
	appliedIDs := make([]string, 0, len(plan.Applied))
	for _, applied := range plan.Applied {
		appliedIDs = append(appliedIDs, applied.ProposalID)
	}
	evalRunIDs := make([]string, 0, len(plan.EvalRuns))
	evalResultIDs := make([]string, 0, plan.EvalResult)
	for _, run := range plan.EvalRuns {
		evalRunIDs = append(evalRunIDs, run.ID)
		for _, result := range run.Cases {
			evalResultIDs = append(evalResultIDs, result.ID)
		}
	}

	for _, marker := range []struct {
		table string
		keys  []string
	}{
		{"approvers", approverIDs},
		{"cases", caseIDs},
		{"agent_runs", runIDs},
		{"proposals", proposalIDs},
		{"approvals", approvalIDs},
		{"eval_runs", evalRunIDs},
		{"eval_case_results", evalResultIDs},
		{"audit_log", auditIDs},
		{"audit_chain_head", auditSeqs},
	} {
		if err := markDemoKeysTx(ctx, tx, marker.table, demoLayerQuality, batchID, marker.keys); err != nil {
			return err
		}
	}
	return markDemoAppliedProposalsTx(ctx, tx, demoLayerQuality, batchID, appliedIDs)
}

func qualityKey(seed int64, key string) string {
	return fmt.Sprintf("%s%d:%s", qualityKeyPrefix, seed, key)
}

func qualityUUID(seed int64, key string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%s", qualityIDNamespace, seed, key)))
	var b [16]byte
	copy(b[:], sum[:16])
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

func qualityHash(seed int64, key string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%s", qualityIDNamespace, seed, key)))
	return hex.EncodeToString(sum[:])
}
