package demo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/db"
)

// M5-04 synthetic ops layer. This file deliberately writes the dashboard demo
// rows directly with SQL after the M1 demo pipeline. It does not run the agent,
// policy engine, approval service, or executor, and it does not write audit
// entries: it is deterministic demo evidence for the v_ops_* panels, not proof
// that the production M2-M4 pipeline ran (Q-98).
const (
	opsIDNamespace   = "recon-demo-ops"
	opsKeyPrefix     = "demo-ops:"
	opsModel         = "demo-ops-synthetic"
	opsPromptVersion = "demo-ops-v1"
)

// ErrOpsDataPresent is returned when the ops layer already seeded the database
// and the caller did not run the documented --reset first. The guard runs
// before the M1 pipeline so a refused rerun leaves every row count unchanged.
var ErrOpsDataPresent = errors.New("demo ops: ops data already present; rerun with --reset")

// OpsSummary is the deterministic `recon demo ops` result. It contains the M1
// summary plus counts only: no database UUIDs are exposed, so two runs on the
// same UTC day print identical text.
type OpsSummary struct {
	M1     Summary
	Seed   int64
	Anchor string

	AgentRuns       int
	Approvers       int
	Proposals       int
	Approvals       int
	Applied         int
	Cases           int
	ResolvedHuman   int
	ResolvedAuto    int
	Escalated       int
	PendingCases    int
	Breaks          int
	ExternalRecords int
	LedgerTxns      int

	AgingBuckets    map[string]int
	Classifications map[string]int
}

// RunOps runs the M1 demo pipeline and then the deterministic M5-04 ops layer
// in one READ COMMITTED transaction that takes EXCLUSIVE locks on every seed
// target table before the presence check. The guard runs under those locks
// before any write: if any ops row already exists, RunOps fails closed with
// ErrOpsDataPresent and nothing is written. A writer that commits before the
// locks is seen by the guard; a writer that starts after the locks blocks until
// the seed commits and cannot contaminate it.
//
// The caller owns --reset (demo.Reset) because it is the existing dev-only
// destructive path. RunOps itself never drops data and never compensates with a
// reset: a failure rolls the one transaction back exactly.
func RunOps(ctx context.Context, opts Options) (OpsSummary, error) {
	opts = opts.withDefaults()
	if strings.TrimSpace(opts.DBURL) == "" {
		return OpsSummary{}, errors.New("demo ops: database URL is required")
	}

	inputs, cleanup, err := prepareSeed(opts)
	if err != nil {
		return OpsSummary{}, err
	}
	defer cleanup()

	pool, err := db.NewPool(ctx, opts.DBURL)
	if err != nil {
		return OpsSummary{}, fmt.Errorf("demo ops: open database: %w", err)
	}
	defer pool.Close()

	var summary OpsSummary
	if err := db.WithTx(ctx, pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			if err := lockDemoSeed(ctx, tx); err != nil {
				return err
			}
			if err := lockSeedTables(ctx, tx); err != nil {
				return err
			}
			present, err := opsDataPresent(ctx, tx)
			if err != nil {
				return fmt.Errorf("demo ops: check existing ops data: %w", err)
			}
			if present {
				return ErrOpsDataPresent
			}
			summary, err = runOpsTx(ctx, opts, tx, inputs)
			return err
		}); err != nil {
		return OpsSummary{}, err
	}
	return summary, nil
}

// runOpsTx runs M1 and the ops layer inside the caller's transaction.
func runOpsTx(ctx context.Context, opts Options, tx pgx.Tx, inputs seedInputs) (OpsSummary, error) {
	m1, err := runSeedTx(ctx, opts, tx, inputs)
	if err != nil {
		return OpsSummary{}, err
	}

	// Anchor every stable day-relative fixture to the database clock, which is
	// also the v_ops_break_aging reference date. The read happens after the M1
	// run so the synthetic fixtures and its break rows share one database day.
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
		return OpsSummary{}, fmt.Errorf("demo ops: read database clock: %w", err)
	}
	now = now.UTC().Truncate(time.Microsecond)

	rc, err := loadOpsContext(ctx, tx)
	if err != nil {
		return OpsSummary{}, err
	}
	plan := buildOpsPlan(opts.Seed, now, opsPlanContext{
		PrimaryCurrency:   rc.primary.Currency,
		SecondaryCurrency: rc.multi.Currency,
		PrimarySource:     rc.primary.Source,
		SecondarySource:   rc.secondarySource,
	})
	batchID, err := newDemoBatchID()
	if err != nil {
		return OpsSummary{}, err
	}
	if err := insertOpsPlan(ctx, tx, plan, rc, batchID); err != nil {
		return OpsSummary{}, err
	}
	return plan.summary(m1), nil
}

// Write prints the M1 summary followed by the ops coverage summary. Both are
// counts-only and contain no UUIDs.
func (s OpsSummary) Write(out io.Writer) error {
	if err := s.M1.Write(out); err != nil {
		return err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "M5-04 ops demo summary: anchor=%s\n", s.Anchor)
	fmt.Fprintf(&b, "  agent_runs=%d approvers=%d proposals=%d approvals=%d applied=%d\n",
		s.AgentRuns, s.Approvers, s.Proposals, s.Approvals, s.Applied)
	fmt.Fprintf(&b, "  cases=%d resolved_human=%d resolved_auto=%d escalated=%d pending=%d\n",
		s.Cases, s.ResolvedHuman, s.ResolvedAuto, s.Escalated, s.PendingCases)
	fmt.Fprintf(&b, "  breaks=%d external_records=%d ledger_txns=%d\n",
		s.Breaks, s.ExternalRecords, s.LedgerTxns)

	bucketOrder := []string{"0-1", "2-3", "4-7", ">7", "unknown"}
	bucketParts := make([]string, 0, len(bucketOrder))
	for _, bucket := range bucketOrder {
		bucketParts = append(bucketParts, fmt.Sprintf("%s=%d", bucket, s.AgingBuckets[bucket]))
	}
	fmt.Fprintf(&b, "  aging %s\n", strings.Join(bucketParts, " "))

	classes := make([]string, 0, len(s.Classifications))
	for class := range s.Classifications {
		classes = append(classes, class)
	}
	sort.Strings(classes)
	classParts := make([]string, 0, len(classes))
	for _, class := range classes {
		classParts = append(classParts, fmt.Sprintf("%s=%d", class, s.Classifications[class]))
	}
	fmt.Fprintf(&b, "  classification %s\n", strings.Join(classParts, " "))

	_, err := io.WriteString(out, b.String())
	return err
}

type opsScope struct {
	ID         string
	Source     string
	Currency   string
	MatchRunID string
}

type opsContext struct {
	primary         opsScope
	multi           opsScope
	secondarySource string
	periodID        string
	debitAccountID  string
	creditAccountID string
}

// loadOpsContext selects the scopes, match runs, period, and ledger accounts
// the synthetic ops rows reuse. It never creates base accounting data.
func loadOpsContext(ctx context.Context, q demoQuerier) (opsContext, error) {
	rows, err := q.Query(ctx,
		`SELECT s.id::text, s.source, s.currency,
		        COALESCE((
		            SELECT mr.id::text
		              FROM match_runs mr
		             WHERE mr.scope_id = s.id AND mr.status = 'succeeded'
		             ORDER BY mr.started_at DESC
		             LIMIT 1
		        ), '')
		   FROM reconciliation_scopes s
		  ORDER BY s.source, s.currency`)
	if err != nil {
		return opsContext{}, fmt.Errorf("demo ops: load scopes: %w", err)
	}
	defer rows.Close()

	var scopes []opsScope
	for rows.Next() {
		var scope opsScope
		if err := rows.Scan(&scope.ID, &scope.Source, &scope.Currency, &scope.MatchRunID); err != nil {
			return opsContext{}, fmt.Errorf("demo ops: scan scope: %w", err)
		}
		scopes = append(scopes, scope)
	}
	if err := rows.Err(); err != nil {
		return opsContext{}, fmt.Errorf("demo ops: iterate scopes: %w", err)
	}

	var rc opsContext
	for _, scope := range scopes {
		if scope.MatchRunID != "" {
			rc.primary = scope
			break
		}
	}
	if rc.primary.ID == "" {
		return opsContext{}, errors.New("demo ops: no succeeded match run found; the M1 demo pipeline must run first")
	}
	for _, scope := range scopes {
		if scope.MatchRunID != "" && scope.Currency != rc.primary.Currency {
			rc.multi = scope
			break
		}
	}
	if rc.multi.ID == "" {
		return opsContext{}, errors.New("demo ops: at least two currencies with succeeded match runs are required")
	}
	rc.secondarySource = rc.primary.Source
	for _, scope := range scopes {
		if scope.MatchRunID != "" && scope.Source != rc.primary.Source {
			rc.secondarySource = scope.Source
			break
		}
	}

	if err := q.QueryRow(ctx,
		`SELECT id::text FROM periods WHERE status = 'open' ORDER BY start_date LIMIT 1`).
		Scan(&rc.periodID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return opsContext{}, errors.New("demo ops: no open period found")
		}
		return opsContext{}, fmt.Errorf("demo ops: load open period: %w", err)
	}

	rc.debitAccountID, rc.creditAccountID, err = loadOpsAccounts(ctx, q, rc.primary.Currency)
	if err != nil {
		return opsContext{}, err
	}
	return rc, nil
}

// loadOpsAccounts finds an active debit/credit account pair for the aging
// fixture's ledger transaction. It prefers the M1 clearing/revenue accounts.
func loadOpsAccounts(ctx context.Context, pool demoQuerier, currency string) (string, string, error) {
	rows, err := pool.Query(ctx,
		`SELECT id::text, type FROM accounts
		  WHERE currency = $1 AND status = 'active'
		  ORDER BY code`, currency)
	if err != nil {
		return "", "", fmt.Errorf("demo ops: load %s accounts: %w", currency, err)
	}
	defer rows.Close()

	var assetIDs, revenueIDs, allIDs []string
	for rows.Next() {
		var id, accountType string
		if err := rows.Scan(&id, &accountType); err != nil {
			return "", "", fmt.Errorf("demo ops: scan %s account: %w", currency, err)
		}
		allIDs = append(allIDs, id)
		switch accountType {
		case "asset":
			assetIDs = append(assetIDs, id)
		case "revenue":
			revenueIDs = append(revenueIDs, id)
		}
	}
	if err := rows.Err(); err != nil {
		return "", "", fmt.Errorf("demo ops: iterate %s accounts: %w", currency, err)
	}
	if len(assetIDs) > 0 && len(revenueIDs) > 0 {
		return assetIDs[0], revenueIDs[0], nil
	}
	if len(allIDs) >= 2 {
		return allIDs[0], allIDs[1], nil
	}
	return "", "", fmt.Errorf("demo ops: at least two active %s accounts are required", currency)
}

type opsApprover struct {
	Key         string
	ID          string
	DisplayName string
	Role        string
	APIKeyID    string
}

type opsCase struct {
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

type opsAgentRun struct {
	Key       string
	ID        string
	CaseID    string
	StartedAt time.Time
	EndedAt   time.Time
	TokensIn  int64
	TokensOut int64
	CostUSD   string
}

type opsProposal struct {
	Key        string
	ID         string
	CaseID     string
	RunID      string
	Type       string
	Status     string
	RiskTier   string
	Confidence float64
	CreatedAt  time.Time
	ExpiresAt  *time.Time
}

type opsApproval struct {
	Key        string
	ID         string
	ProposalID string
	ApproverID string
	Decision   string
	DecidedAt  time.Time
}

type opsApplied struct {
	ProposalID    string
	AppliedAt     time.Time
	ExecutorRunID string
}

type opsBatch struct {
	Key         string
	ID          string
	Source      string
	Currency    string
	FileHash    string
	PeriodStart time.Time
	PeriodEnd   time.Time
	RowCount    int
}

type opsExternalRecord struct {
	Key            string
	ID             string
	BatchKey       string
	Source         string
	SourceRecordID string
	Currency       string
	AmountMinor    int64
	OccurredAt     time.Time
	ValueDate      time.Time
}

type opsRecordRef struct {
	Source         string `json:"source"`
	SourceRecordID string `json:"source_record_id"`
}

type opsBreak struct {
	Key          string
	ID           string
	Signature    string
	ScopeRole    string
	Side         string
	Currency     string
	LedgerTxnKey string
	ExternalRefs []opsRecordRef
	DeltaMinor   int64
	DetectedAt   time.Time
	MinValueDate *time.Time
}

type opsLedgerTxn struct {
	Key         string
	ID          string
	Currency    string
	AmountMinor int64
	ValueDate   time.Time
}

type opsPlanContext struct {
	PrimaryCurrency   string
	SecondaryCurrency string
	PrimarySource     string
	SecondarySource   string
}

type opsPlan struct {
	Seed            int64
	Anchor          time.Time
	Approvers       []opsApprover
	Cases           []opsCase
	Runs            []opsAgentRun
	Proposals       []opsProposal
	Approvals       []opsApproval
	Applied         []opsApplied
	Batches         []opsBatch
	ExternalRecords []opsExternalRecord
	LedgerTxns      []opsLedgerTxn
	Breaks          []opsBreak
}

// buildOpsPlan is pure: the same seed, UTC day, and curated context always
// produce the same deterministic IDs, timestamps, and coverage counts. It is
// unit-tested without a database.
func buildOpsPlan(seed int64, now time.Time, cfg opsPlanContext) opsPlan {
	now = now.UTC().Truncate(time.Microsecond)
	anchor := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	at := func(dayOffset, hour int) time.Time {
		return anchor.AddDate(0, 0, dayOffset).Add(time.Duration(hour) * time.Hour)
	}
	ptrTime := func(t time.Time) *time.Time { return &t }
	ptrString := func(s string) *string { return &s }
	id := func(key string) string { return opsUUID(seed, key) }

	plan := opsPlan{
		Seed:   seed,
		Anchor: anchor,
		Approvers: []opsApprover{
			{
				Key:         "approver-1",
				ID:          id("approver:1"),
				DisplayName: "Demo Ops Approver One",
				Role:        "approver",
				APIKeyID:    opsKey(seed, "approver-1"),
			},
			{
				Key:         "approver-2",
				ID:          id("approver:2"),
				DisplayName: "Demo Ops Approver Two",
				Role:        "approver",
				APIKeyID:    opsKey(seed, "approver-2"),
			},
		},
	}

	caseSpecs := []opsCase{
		{
			Key: "case-human-1", Priority: 2, ValueMinor: 120_000, Currency: cfg.PrimaryCurrency,
			Status: "resolved", CreatedAt: at(-4, 6), ResolvedAt: ptrTime(at(-4, 12)),
			ResolutionType: ptrString("human_approved"),
		},
		{
			Key: "case-human-2", Priority: 2, ValueMinor: 90_000, Currency: cfg.SecondaryCurrency,
			Status: "resolved", CreatedAt: at(-3, 6), ResolvedAt: ptrTime(at(-3, 12)),
			ResolutionType: ptrString("human_approved"),
		},
		{
			Key: "case-auto-1", Priority: 1, ValueMinor: 4_500, Currency: cfg.PrimaryCurrency,
			Status: "resolved", CreatedAt: at(-1, 6), ResolvedAt: ptrTime(at(-1, 12)),
			ResolutionType: ptrString("auto_applied"),
		},
		{
			Key: "case-auto-2", Priority: 1, ValueMinor: 3_200, Currency: cfg.SecondaryCurrency,
			Status: "resolved", CreatedAt: at(-2, 6), ResolvedAt: ptrTime(at(-2, 12)),
			ResolutionType: ptrString("auto_applied"),
		},
		{
			Key: "case-escalated-1", Priority: 3, ValueMinor: 250_000, Currency: cfg.PrimaryCurrency,
			Status: "escalated", CreatedAt: at(-5, 12),
			EscalationReason: ptrString("high_value_requires_four_eyes"),
		},
		{
			Key: "case-escalated-2", Priority: 3, ValueMinor: 180_000, Currency: cfg.SecondaryCurrency,
			Status: "escalated", CreatedAt: at(-6, 12),
			EscalationReason: ptrString("verifier_missing"),
		},
		{
			Key: "case-pending-1", Priority: 1, ValueMinor: 30_000, Currency: cfg.PrimaryCurrency,
			Status: "proposal_pending", CreatedAt: now.Add(-2 * time.Hour).Truncate(time.Second),
		},
		{
			Key: "case-pending-2", Priority: 2, ValueMinor: 45_000, Currency: cfg.SecondaryCurrency,
			Status: "proposal_pending", CreatedAt: now.Add(-4 * time.Hour).Truncate(time.Second),
		},
		{
			Key: "case-rejected-1", Priority: 1, ValueMinor: 12_000, Currency: cfg.PrimaryCurrency,
			Status: "waiting_info", CreatedAt: at(-2, 9),
		},
	}
	caseID := make(map[string]string, len(caseSpecs))
	for i := range caseSpecs {
		caseSpecs[i].ID = id("case:" + caseSpecs[i].Key)
		caseID[caseSpecs[i].Key] = caseSpecs[i].ID
	}
	plan.Cases = caseSpecs

	type approvalSpec struct {
		ApproverKey string
		Decision    string
		DecidedAt   time.Time
	}
	type proposalSpec struct {
		Key        string
		CaseKey    string
		Type       string
		Status     string
		RiskTier   string
		Confidence float64
		CreatedAt  time.Time
		Approval   *approvalSpec
		AppliedAt  *time.Time
	}
	latency := func(created time.Time, d time.Duration) time.Time { return created.Add(d) }
	humanOneCreated := at(-4, 8)
	humanTwoCreated := at(-3, 9)
	precedenceCreated := at(-5, 14)
	rejectedCreated := at(-2, 10)
	autoOneCreated := at(-1, 8)
	autoTwoCreated := at(-2, 8)
	pendingOneCreated := now.Add(-90 * time.Minute).Truncate(time.Second)
	pendingTwoCreated := now.Add(-3 * time.Hour).Truncate(time.Second)

	proposalSpecs := []proposalSpec{
		{
			Key: "proposal-auto-1", CaseKey: "case-auto-1", Type: "link", Status: "applied",
			RiskTier: "T0", Confidence: 0.98, CreatedAt: autoOneCreated,
			AppliedAt: ptrTime(autoOneCreated.Add(30 * time.Minute)),
		},
		{
			Key: "proposal-auto-2", CaseKey: "case-auto-2", Type: "link", Status: "applied",
			RiskTier: "T0", Confidence: 0.97, CreatedAt: autoTwoCreated,
			AppliedAt: ptrTime(autoTwoCreated.Add(30 * time.Minute)),
		},
		{
			Key: "proposal-human-1", CaseKey: "case-human-1", Type: "link", Status: "applied",
			RiskTier: "T1", Confidence: 0.91, CreatedAt: humanOneCreated,
			Approval:  &approvalSpec{ApproverKey: "approver-1", Decision: "approved", DecidedAt: latency(humanOneCreated, 3*time.Hour)},
			AppliedAt: ptrTime(latency(humanOneCreated, 3*time.Hour+30*time.Minute)),
		},
		{
			Key: "proposal-human-2", CaseKey: "case-human-2", Type: "link", Status: "applied",
			RiskTier: "T1", Confidence: 0.88, CreatedAt: humanTwoCreated,
			Approval:  &approvalSpec{ApproverKey: "approver-2", Decision: "approved", DecidedAt: latency(humanTwoCreated, 2*time.Hour)},
			AppliedAt: ptrTime(latency(humanTwoCreated, 2*time.Hour+30*time.Minute)),
		},
		{
			Key: "proposal-pending-1", CaseKey: "case-pending-1", Type: "link", Status: "pending_approval",
			RiskTier: "T1", Confidence: 0.84, CreatedAt: pendingOneCreated,
		},
		{
			Key: "proposal-pending-2", CaseKey: "case-pending-2", Type: "link", Status: "pending_approval",
			RiskTier: "T2", Confidence: 0.79, CreatedAt: pendingTwoCreated,
		},
		{
			Key: "proposal-rejected-1", CaseKey: "case-rejected-1", Type: "write_off", Status: "rejected",
			RiskTier: "T2", Confidence: 0.62, CreatedAt: rejectedCreated,
			Approval: &approvalSpec{ApproverKey: "approver-2", Decision: "rejected", DecidedAt: latency(rejectedCreated, time.Hour)},
		},
		{
			Key: "proposal-precedence-1", CaseKey: "case-escalated-1", Type: "link", Status: "applied",
			RiskTier: "T2", Confidence: 0.81, CreatedAt: precedenceCreated,
			Approval:  &approvalSpec{ApproverKey: "approver-1", Decision: "approved", DecidedAt: latency(precedenceCreated, 3*time.Hour)},
			AppliedAt: ptrTime(latency(precedenceCreated, 3*time.Hour+30*time.Minute)),
		},
	}
	approverID := map[string]string{
		"approver-1": plan.Approvers[0].ID,
		"approver-2": plan.Approvers[1].ID,
	}
	for i, spec := range proposalSpecs {
		proposalID := id("proposal:" + spec.Key)
		runID := id("run:" + spec.Key)
		runStart := spec.CreatedAt.Add(-2 * time.Minute)
		runEnd := spec.CreatedAt.Add(-10 * time.Second)
		plan.Runs = append(plan.Runs, opsAgentRun{
			Key:       spec.Key,
			ID:        runID,
			CaseID:    caseID[spec.CaseKey],
			StartedAt: runStart,
			EndedAt:   runEnd,
			TokensIn:  int64(800 + i*137),
			TokensOut: int64(120 + i*29),
			CostUSD:   fmt.Sprintf("0.%06d", 8000+i*111),
		})
		plan.Proposals = append(plan.Proposals, opsProposal{
			Key:        spec.Key,
			ID:         proposalID,
			CaseID:     caseID[spec.CaseKey],
			RunID:      runID,
			Type:       spec.Type,
			Status:     spec.Status,
			RiskTier:   spec.RiskTier,
			Confidence: spec.Confidence,
			CreatedAt:  spec.CreatedAt,
			ExpiresAt:  ptrTime(spec.CreatedAt.Add(48 * time.Hour)),
		})
		if spec.Approval != nil {
			plan.Approvals = append(plan.Approvals, opsApproval{
				Key:        spec.Key,
				ID:         id("approval:" + spec.Key),
				ProposalID: proposalID,
				ApproverID: approverID[spec.Approval.ApproverKey],
				Decision:   spec.Approval.Decision,
				DecidedAt:  spec.Approval.DecidedAt,
			})
		}
		if spec.AppliedAt != nil {
			plan.Applied = append(plan.Applied, opsApplied{
				ProposalID:    proposalID,
				AppliedAt:     *spec.AppliedAt,
				ExecutorRunID: opsKey(seed, "exec:"+spec.Key),
			})
		}
	}

	batchIDs := make(map[string]string)
	batchRows := make(map[string]int)
	addBatch := func(source, currency string) *opsBatch {
		key := source + "|" + currency
		if _, ok := batchIDs[key]; ok {
			return nil
		}
		batch := opsBatch{
			Key:         key,
			ID:          id("batch:" + key),
			Source:      source,
			Currency:    currency,
			FileHash:    opsKey(seed, "batch:"+key),
			PeriodStart: anchor.AddDate(0, 0, -70),
			PeriodEnd:   anchor.AddDate(0, 0, 10),
		}
		batchIDs[key] = batch.ID
		plan.Batches = append(plan.Batches, batch)
		return &plan.Batches[len(plan.Batches)-1]
	}
	_ = addBatch(cfg.PrimarySource, cfg.PrimaryCurrency)
	_ = addBatch(cfg.SecondarySource, cfg.PrimaryCurrency)
	_ = addBatch(cfg.PrimarySource, cfg.SecondaryCurrency)

	recordSpecs := []opsExternalRecord{
		{
			Key: "ext-primary-0", Source: cfg.PrimarySource, Currency: cfg.PrimaryCurrency,
			AmountMinor: 1_250, OccurredAt: at(0, 12), ValueDate: anchor,
		},
		{
			Key: "ext-primary-2", Source: cfg.PrimarySource, Currency: cfg.PrimaryCurrency,
			AmountMinor: 2_400, OccurredAt: at(-2, 12), ValueDate: anchor.AddDate(0, 0, -2),
		},
		{
			Key: "ext-primary-5", Source: cfg.PrimarySource, Currency: cfg.PrimaryCurrency,
			AmountMinor: 3_750, OccurredAt: at(-5, 12), ValueDate: anchor.AddDate(0, 0, -5),
		},
		{
			Key: "ext-primary-60", Source: cfg.PrimarySource, Currency: cfg.PrimaryCurrency,
			AmountMinor: 8_600, OccurredAt: at(-60, 12), ValueDate: anchor.AddDate(0, 0, -60),
		},
		{
			Key: "ext-secondary-5", Source: cfg.SecondarySource, Currency: cfg.PrimaryCurrency,
			AmountMinor: 5_100, OccurredAt: at(-5, 12), ValueDate: anchor.AddDate(0, 0, -5),
		},
		{
			Key: "ext-secondary-future", Source: cfg.SecondarySource, Currency: cfg.PrimaryCurrency,
			AmountMinor: 9_500, OccurredAt: at(5, 12), ValueDate: anchor.AddDate(0, 0, 5),
		},
		{
			Key: "ext-primary-eur-2", Source: cfg.PrimarySource, Currency: cfg.SecondaryCurrency,
			AmountMinor: 4_300, OccurredAt: at(-2, 12), ValueDate: anchor.AddDate(0, 0, -2),
		},
	}
	recordValueDate := make(map[string]time.Time, len(recordSpecs))
	for _, spec := range recordSpecs {
		batchKey := spec.Source + "|" + spec.Currency
		record := spec
		record.ID = id("external:" + spec.Key)
		record.BatchKey = batchKey
		record.SourceRecordID = opsKey(seed, "ext:"+spec.Key)
		plan.ExternalRecords = append(plan.ExternalRecords, record)
		recordValueDate[spec.Key] = spec.ValueDate
		batchRows[batchKey]++
	}
	for i := range plan.Batches {
		plan.Batches[i].RowCount = batchRows[plan.Batches[i].Key]
	}

	ledgerValueDate := anchor
	plan.LedgerTxns = []opsLedgerTxn{{
		Key:         "ledger-primary-0",
		ID:          id("ledger:primary-0"),
		Currency:    cfg.PrimaryCurrency,
		AmountMinor: 1_250,
		ValueDate:   ledgerValueDate,
	}}

	ref := func(key string) opsRecordRef {
		for _, record := range plan.ExternalRecords {
			if record.Key == key {
				return opsRecordRef{Source: record.Source, SourceRecordID: record.SourceRecordID}
			}
		}
		return opsRecordRef{}
	}
	breaks := []opsBreak{
		{
			Key: "break-ledger-0", ScopeRole: "primary", Side: "ledger_only",
			Currency: cfg.PrimaryCurrency, LedgerTxnKey: plan.LedgerTxns[0].Key,
			DeltaMinor: 1_250, DetectedAt: at(-1, 18), MinValueDate: ptrTime(ledgerValueDate),
		},
		{
			Key: "break-external-2", ScopeRole: "primary", Side: "external_only",
			Currency: cfg.PrimaryCurrency, ExternalRefs: []opsRecordRef{ref("ext-primary-2")},
			DeltaMinor: -2_400, DetectedAt: at(-2, 18), MinValueDate: ptrTime(recordValueDate["ext-primary-2"]),
		},
		{
			Key: "break-mismatch-5", ScopeRole: "primary", Side: "amount_mismatch",
			Currency: cfg.PrimaryCurrency, ExternalRefs: []opsRecordRef{ref("ext-primary-5")},
			DeltaMinor: 3_750, DetectedAt: at(-5, 18), MinValueDate: ptrTime(recordValueDate["ext-primary-5"]),
		},
		{
			Key: "break-external-60", ScopeRole: "primary", Side: "external_only",
			Currency: cfg.PrimaryCurrency, ExternalRefs: []opsRecordRef{ref("ext-primary-60")},
			DeltaMinor: -8_600, DetectedAt: at(-60, 18), MinValueDate: ptrTime(recordValueDate["ext-primary-60"]),
		},
		{
			Key: "break-multi-ref", ScopeRole: "primary", Side: "external_only",
			Currency:     cfg.PrimaryCurrency,
			ExternalRefs: []opsRecordRef{ref("ext-primary-60"), ref("ext-secondary-5")},
			DeltaMinor:   5_100, DetectedAt: at(-5, 18), MinValueDate: ptrTime(recordValueDate["ext-primary-60"]),
		},
		{
			Key: "break-unknown-ref", ScopeRole: "primary", Side: "external_only",
			Currency: cfg.PrimaryCurrency,
			ExternalRefs: []opsRecordRef{{
				Source: cfg.PrimarySource, SourceRecordID: opsKey(seed, "ext:missing"),
			}},
			DeltaMinor: -700, DetectedAt: at(-1, 19),
		},
		{
			Key: "break-future-ref", ScopeRole: "primary", Side: "amount_mismatch",
			Currency: cfg.PrimaryCurrency, ExternalRefs: []opsRecordRef{ref("ext-secondary-future")},
			DeltaMinor: 9_500, DetectedAt: at(-1, 20), MinValueDate: ptrTime(recordValueDate["ext-secondary-future"]),
		},
		{
			Key: "break-multi-currency", ScopeRole: "multi", Side: "external_only",
			Currency: cfg.SecondaryCurrency, ExternalRefs: []opsRecordRef{ref("ext-primary-eur-2")},
			DeltaMinor: -4_300, DetectedAt: at(-2, 19), MinValueDate: ptrTime(recordValueDate["ext-primary-eur-2"]),
		},
	}
	for i := range breaks {
		breaks[i].ID = id("break:" + breaks[i].Key)
		breaks[i].Signature = opsSignature(seed, "break:"+breaks[i].Key)
	}
	plan.Breaks = breaks
	return plan
}

func (p opsPlan) summary(m1 Summary) OpsSummary {
	counts := opsCounts{AgingBuckets: map[string]int{}, Classifications: map[string]int{}}
	counts.Approvers = len(p.Approvers)
	counts.Cases = len(p.Cases)
	counts.AgentRuns = len(p.Runs)
	counts.Proposals = len(p.Proposals)
	counts.Approvals = len(p.Approvals)
	counts.Applied = len(p.Applied)
	counts.Breaks = len(p.Breaks)
	counts.ExternalRecords = len(p.ExternalRecords)
	counts.LedgerTxns = len(p.LedgerTxns)
	for _, c := range p.Cases {
		switch c.Status {
		case "proposal_pending":
			counts.PendingCases++
		case "escalated":
			counts.Escalated++
		}
	}

	approvedProposal := make(map[string]bool, len(p.Approvals))
	for _, approval := range p.Approvals {
		if approval.Decision == "approved" {
			approvedProposal[approval.ProposalID] = true
		}
	}
	appliedProposal := make(map[string]bool, len(p.Applied))
	for _, applied := range p.Applied {
		appliedProposal[applied.ProposalID] = true
	}
	for _, c := range p.Cases {
		if c.Status != "resolved" {
			continue
		}
		human, auto := false, false
		for _, proposal := range p.Proposals {
			if proposal.CaseID != c.ID {
				continue
			}
			if approvedProposal[proposal.ID] {
				human = true
			}
			if (proposal.Status == "auto_approved" || proposal.Status == "applied") &&
				appliedProposal[proposal.ID] && !approvedProposal[proposal.ID] {
				auto = true
			}
		}
		switch {
		case human:
			counts.ResolvedHuman++
		case auto:
			counts.ResolvedAuto++
		}
	}
	for _, c := range p.Cases {
		class := ""
		switch c.Status {
		case "escalated":
			class = "escalated"
		case "resolved":
			for _, proposal := range p.Proposals {
				if proposal.CaseID == c.ID && approvedProposal[proposal.ID] {
					class = "human"
					break
				}
			}
			if class == "" {
				for _, proposal := range p.Proposals {
					if proposal.CaseID == c.ID && appliedProposal[proposal.ID] &&
						(proposal.Status == "auto_approved" || proposal.Status == "applied") &&
						!approvedProposal[proposal.ID] {
						class = "auto"
						break
					}
				}
			}
		}
		if class != "" {
			counts.Classifications[class]++
		}
	}
	for _, b := range p.Breaks {
		bucket, _ := opsAgingBucket(p.Anchor, b.MinValueDate)
		counts.AgingBuckets[bucket]++
	}

	return OpsSummary{
		M1:              m1,
		Seed:            p.Seed,
		Anchor:          p.Anchor.Format("2006-01-02"),
		AgentRuns:       counts.AgentRuns,
		Approvers:       counts.Approvers,
		Proposals:       counts.Proposals,
		Approvals:       counts.Approvals,
		Applied:         counts.Applied,
		Cases:           counts.Cases,
		ResolvedHuman:   counts.ResolvedHuman,
		ResolvedAuto:    counts.ResolvedAuto,
		Escalated:       counts.Escalated,
		PendingCases:    counts.PendingCases,
		Breaks:          counts.Breaks,
		ExternalRecords: counts.ExternalRecords,
		LedgerTxns:      counts.LedgerTxns,
		AgingBuckets:    counts.AgingBuckets,
		Classifications: counts.Classifications,
	}
}

type opsCounts struct {
	AgentRuns       int
	Approvers       int
	Proposals       int
	Approvals       int
	Applied         int
	Cases           int
	ResolvedHuman   int
	ResolvedAuto    int
	Escalated       int
	PendingCases    int
	Breaks          int
	ExternalRecords int
	LedgerTxns      int
	AgingBuckets    map[string]int
	Classifications map[string]int
}

// opsAgingBucket mirrors v_ops_break_aging: no resolvable date and future
// dates are both 'unknown' (order 5), never '0-1'.
func opsAgingBucket(anchor time.Time, valueDate *time.Time) (string, int) {
	if valueDate == nil || valueDate.After(anchor) {
		return "unknown", 5
	}
	days := int(anchor.Sub(*valueDate).Hours() / 24)
	switch {
	case days <= 1:
		return "0-1", 1
	case days <= 3:
		return "2-3", 2
	case days <= 7:
		return "4-7", 3
	default:
		return ">7", 4
	}
}

func opsDataPresent(ctx context.Context, q demoQuerier) (bool, error) {
	return opsDataPresentQuerier(ctx, q)
}

type opsRowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func opsDataPresentQuerier(ctx context.Context, q opsRowQuerier) (bool, error) {
	var present bool
	err := q.QueryRow(ctx,
		`SELECT EXISTS (
		    SELECT 1 FROM proposals WHERE idempotency_key LIKE $1
		    UNION ALL
		    SELECT 1 FROM agent_runs WHERE model = $2
		    UNION ALL
		    SELECT 1 FROM approvers WHERE api_key_id LIKE $1
		    UNION ALL
		    SELECT 1 FROM settlement_batches WHERE file_hash LIKE $1
		)`, opsKeyPrefix+"%", opsModel).Scan(&present)
	if err != nil {
		return false, err
	}
	return present, nil
}

func insertOpsPlan(ctx context.Context, tx demoQuerier, plan opsPlan, rc opsContext, batchID string) error {
	present, err := opsDataPresentQuerier(ctx, tx)
	if err != nil {
		return fmt.Errorf("demo ops: re-check existing ops data: %w", err)
	}
	if present {
		return ErrOpsDataPresent
	}

	for _, approver := range plan.Approvers {
		if _, err := tx.Exec(ctx,
			`INSERT INTO approvers (id, display_name, role, api_key_id)
				 VALUES ($1::uuid, $2, $3, $4)`,
			approver.ID, approver.DisplayName, approver.Role, approver.APIKeyID); err != nil {
			return fmt.Errorf("demo ops: insert approver %s: %w", approver.Key, err)
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
			return fmt.Errorf("demo ops: insert case %s: %w", c.Key, err)
		}
	}
	for _, run := range plan.Runs {
		if _, err := tx.Exec(ctx,
			`INSERT INTO agent_runs
				     (id, case_id, model, prompt_version, started_at, ended_at,
				      tokens_in, tokens_out, cost_usd, trace_id, outcome)
				 VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8, $9::numeric, NULL, 'proposal')`,
			run.ID, run.CaseID, opsModel, opsPromptVersion, run.StartedAt, run.EndedAt,
			run.TokensIn, run.TokensOut, run.CostUSD); err != nil {
			return fmt.Errorf("demo ops: insert agent run %s: %w", run.Key, err)
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
			"Demo ops synthetic proposal for the M5-04 dashboard.",
			proposal.Confidence, proposal.RiskTier, proposal.Status,
			opsSignature(plan.Seed, "state:"+proposal.Key),
			opsKey(plan.Seed, "proposal:"+proposal.Key),
			proposal.CreatedAt, proposal.ExpiresAt); err != nil {
			return fmt.Errorf("demo ops: insert proposal %s: %w", proposal.Key, err)
		}
	}
	for _, approval := range plan.Approvals {
		if _, err := tx.Exec(ctx,
			`INSERT INTO approvals
				     (id, proposal_id, channel, approver_id, decision, decided_at, signature_valid)
				 VALUES ($1::uuid, $2::uuid, 'demo-seed', $3::uuid, $4, $5, true)`,
			approval.ID, approval.ProposalID, approval.ApproverID,
			approval.Decision, approval.DecidedAt); err != nil {
			return fmt.Errorf("demo ops: insert approval %s: %w", approval.Key, err)
		}
	}
	for _, applied := range plan.Applied {
		if _, err := tx.Exec(ctx,
			`INSERT INTO applied_proposals
				     (proposal_id, ledger_txn_id, applied_at, executor_run_id)
				 VALUES ($1::uuid, NULL, $2, $3)`,
			applied.ProposalID, applied.AppliedAt, applied.ExecutorRunID); err != nil {
			return fmt.Errorf("demo ops: insert applied proposal: %w", err)
		}
	}
	for _, batch := range plan.Batches {
		if _, err := tx.Exec(ctx,
			`INSERT INTO settlement_batches
				     (id, source, period_start, period_end, declared_total_minor,
				      currency, file_hash, status, row_count, declared_row_count)
				 VALUES ($1::uuid, $2, $3::date, $4::date, 0, $5, $6, 'ingested', $7, $7)`,
			batch.ID, batch.Source, batch.PeriodStart, batch.PeriodEnd,
			batch.Currency, batch.FileHash, batch.RowCount); err != nil {
			return fmt.Errorf("demo ops: insert batch %s: %w", batch.Key, err)
		}
	}
	batchLookup := make(map[string]string, len(plan.Batches))
	for _, batch := range plan.Batches {
		batchLookup[batch.Key] = batch.ID
	}
	for _, record := range plan.ExternalRecords {
		if _, err := tx.Exec(ctx,
			`INSERT INTO external_records
				     (id, source, source_record_id, kind, amount_minor, currency,
				      fee_minor, occurred_at, settled_at, value_date, batch_id,
				      raw, raw_hash)
				 VALUES ($1::uuid, $2, $3, 'charge', $4, $5, 0, $6, NULL,
				         $7::date, $8::uuid, '{}'::jsonb, $9)`,
			record.ID, record.Source, record.SourceRecordID, record.AmountMinor,
			record.Currency, record.OccurredAt, record.ValueDate,
			batchLookup[record.BatchKey], opsKey(plan.Seed, "raw:"+record.Key)); err != nil {
			return fmt.Errorf("demo ops: insert external record %s: %w", record.Key, err)
		}
	}
	ledgerID := make(map[string]string, len(plan.LedgerTxns))
	for _, txn := range plan.LedgerTxns {
		if _, err := tx.Exec(ctx,
			`INSERT INTO ledger_txns
				     (id, value_date, kind, idempotency_key, request_hash,
				      description, period_id)
				 VALUES ($1::uuid, $2::date, 'charge', $3, $4,
				         'demo ops aging fixture', $5::uuid)`,
			txn.ID, txn.ValueDate, opsKey(plan.Seed, "ledger:"+txn.Key),
			opsSignature(plan.Seed, "ledger-request:"+txn.Key), rc.periodID); err != nil {
			return fmt.Errorf("demo ops: insert ledger txn %s: %w", txn.Key, err)
		}
		ledgerID[txn.Key] = txn.ID
		if _, err := tx.Exec(ctx,
			`INSERT INTO ledger_entries
				     (txn_id, account_id, direction, amount_minor, currency)
				 VALUES ($1::uuid, $2::uuid, 'debit', $3, $4)`,
			txn.ID, rc.debitAccountID, txn.AmountMinor, txn.Currency); err != nil {
			return fmt.Errorf("demo ops: insert ledger debit entry: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO ledger_entries
				     (txn_id, account_id, direction, amount_minor, currency)
				 VALUES ($1::uuid, $2::uuid, 'credit', $3, $4)`,
			txn.ID, rc.creditAccountID, txn.AmountMinor, txn.Currency); err != nil {
			return fmt.Errorf("demo ops: insert ledger credit entry: %w", err)
		}
	}
	for _, breakRow := range plan.Breaks {
		scope := rc.primary
		if breakRow.ScopeRole == "multi" {
			scope = rc.multi
		}
		ledgerIDs := []string{}
		if breakRow.LedgerTxnKey != "" {
			ledgerIDs = append(ledgerIDs, ledgerID[breakRow.LedgerTxnKey])
		}
		refs := breakRow.ExternalRefs
		if refs == nil {
			refs = []opsRecordRef{}
		}
		refsJSON, err := json.Marshal(refs)
		if err != nil {
			return fmt.Errorf("demo ops: encode break refs: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO breaks
				     (id, signature, scope_id, match_run_id, side, ledger_txn_ids,
				      external_refs, delta_minor, currency, status, detected_at)
				 VALUES ($1::uuid, $2, $3::uuid, $4::uuid, $5, $6::text[]::uuid[],
				         $7::jsonb, $8, $9, 'open', $10)`,
			breakRow.ID, breakRow.Signature, scope.ID, scope.MatchRunID,
			breakRow.Side, ledgerIDs, string(refsJSON), breakRow.DeltaMinor,
			breakRow.Currency, breakRow.DetectedAt); err != nil {
			return fmt.Errorf("demo ops: insert break %s: %w", breakRow.Key, err)
		}
	}
	return markOpsPlanRows(ctx, tx, plan, batchID)
}

// markOpsPlanRows records every row written by this ops plan inside the same
// transaction. A reset can then prove the rows are demo-owned.
func markOpsPlanRows(ctx context.Context, tx demoQuerier, plan opsPlan, batchID string) error {
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
	batchIDs := make([]string, 0, len(plan.Batches))
	for _, batch := range plan.Batches {
		batchIDs = append(batchIDs, batch.ID)
	}
	recordIDs := make([]string, 0, len(plan.ExternalRecords))
	for _, record := range plan.ExternalRecords {
		recordIDs = append(recordIDs, record.ID)
	}
	txnIDs := make([]string, 0, len(plan.LedgerTxns))
	for _, txn := range plan.LedgerTxns {
		txnIDs = append(txnIDs, txn.ID)
	}
	breakIDs := make([]string, 0, len(plan.Breaks))
	for _, breakRow := range plan.Breaks {
		breakIDs = append(breakIDs, breakRow.ID)
	}
	appliedIDs := make([]string, 0, len(plan.Applied))
	for _, applied := range plan.Applied {
		appliedIDs = append(appliedIDs, applied.ProposalID)
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
		{"settlement_batches", batchIDs},
		{"external_records", recordIDs},
		{"ledger_txns", txnIDs},
		{"breaks", breakIDs},
	} {
		if err := markDemoKeysTx(ctx, tx, marker.table, demoLayerOps, batchID, marker.keys); err != nil {
			return err
		}
	}
	if err := markDemoAppliedProposalsTx(ctx, tx, demoLayerOps, batchID, appliedIDs); err != nil {
		return err
	}
	return markDemoLedgerEntriesTx(ctx, tx, demoLayerOps, batchID, txnIDs)
}

func opsKey(seed int64, key string) string {
	return fmt.Sprintf("%s%d:%s", opsKeyPrefix, seed, key)
}

func opsUUID(seed int64, key string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%s", opsIDNamespace, seed, key)))
	var b [16]byte
	copy(b[:], sum[:16])
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

func opsSignature(seed int64, key string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%s", opsIDNamespace, seed, key)))
	return hex.EncodeToString(sum[:])
}
