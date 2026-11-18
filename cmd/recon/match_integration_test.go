//go:build integration

// covers: FIN-24, FIN-30

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/eval/fixtures/m2"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

type cliUnmatchWorld struct {
	matchID     string
	ledgerTxnID string
	proposalID  string
}

// seedCLIUnmatchWorld turns the fixed M2 fixture into one active two-leg match
// with two linked breaks/cases and (optionally) an applied adjustment. The
// match row is inserted directly here because this test exercises the CLI
// boundary, not the matcher; the service integration test uses the real
// matcher for matchLink coverage.
func seedCLIUnmatchWorld(t *testing.T, d *testdb.DB, applied bool) cliUnmatchWorld {
	t.Helper()
	ctx := context.Background()
	var (
		scopeID, runID, externalID string
		feeBreakID, refBreakID     string
		feeLedgerID, refLedgerID   string
	)
	if err := d.Pool.QueryRow(ctx,
		`SELECT id::text FROM reconciliation_scopes WHERE source = 'psp_m2'`).
		Scan(&scopeID); err != nil {
		t.Fatalf("load m2 scope: %v", err)
	}
	if err := d.Pool.QueryRow(ctx,
		`SELECT id::text FROM match_runs WHERE scope_id = $1::uuid`,
		scopeID).Scan(&runID); err != nil {
		t.Fatalf("load m2 match run: %v", err)
	}
	if err := d.Pool.QueryRow(ctx,
		`SELECT id::text FROM external_records
		  WHERE source = 'psp_m2' AND source_record_id = 'fee-1'`).
		Scan(&externalID); err != nil {
		t.Fatalf("load m2 external record: %v", err)
	}
	if err := d.Pool.QueryRow(ctx,
		`SELECT b.id::text, b.ledger_txn_ids[1]::text
		   FROM breaks b
		   JOIN case_breaks cb ON cb.break_id = b.id
		  WHERE cb.case_id = $1::uuid`, m2.FeeVarianceCaseID).
		Scan(&feeBreakID, &feeLedgerID); err != nil {
		t.Fatalf("load fee-variance break: %v", err)
	}
	if err := d.Pool.QueryRow(ctx,
		`SELECT b.id::text, b.ledger_txn_ids[1]::text
		   FROM breaks b
		   JOIN case_breaks cb ON cb.break_id = b.id
		  WHERE cb.case_id = $1::uuid`, m2.ReferenceTypoCaseID).
		Scan(&refBreakID, &refLedgerID); err != nil {
		t.Fatalf("load reference-typo break: %v", err)
	}

	var matchID string
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO matches
		     (scope_id, external_record_id, rule, confidence, match_run_id, status, created_at)
		 VALUES ($1::uuid, $2::uuid, 'cli-unmatch', 1.0, $3::uuid, 'active', now())
		 RETURNING id::text`, scopeID, externalID, runID).Scan(&matchID); err != nil {
		t.Fatalf("insert match: %v", err)
	}
	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO match_legs (match_id, ledger_txn_id, amount_minor, currency, status)
		 VALUES ($1::uuid, $2::uuid, 10000, 'USD', 'active'),
		        ($1::uuid, $3::uuid, 5000, 'USD', 'active')`,
		matchID, feeLedgerID, refLedgerID); err != nil {
		t.Fatalf("insert match legs: %v", err)
	}
	if _, err := d.Pool.Exec(ctx,
		`UPDATE breaks SET status = 'in_case' WHERE id = $1::uuid`, feeBreakID); err != nil {
		t.Fatalf("set fee break in_case: %v", err)
	}
	if _, err := d.Pool.Exec(ctx,
		`UPDATE breaks SET status = 'resolved' WHERE id = $1::uuid`, refBreakID); err != nil {
		t.Fatalf("set reference break resolved: %v", err)
	}
	if _, err := d.Pool.Exec(ctx,
		`UPDATE cases
		    SET status = 'resolved', resolved_at = now(), resolution_type = 'manual'
		  WHERE id = $1::uuid`, m2.FeeVarianceCaseID); err != nil {
		t.Fatalf("resolve fee-variance case: %v", err)
	}
	if _, err := d.Pool.Exec(ctx,
		`UPDATE cases
		    SET status = 'proposal_pending', resolved_at = NULL, resolution_type = NULL
		  WHERE id = $1::uuid`, m2.ReferenceTypoCaseID); err != nil {
		t.Fatalf("set reference-typo case proposal_pending: %v", err)
	}

	proposalID := seedCLIProposal(t, d, m2.ReferenceTypoCaseID, "link", "proposed",
		"cli-unmatch-proposed")
	if applied {
		appliedID := seedCLIProposal(t, d, m2.ReferenceTypoCaseID, "adjustment", "applied",
			"cli-unmatch-applied")
		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO applied_proposals
			     (proposal_id, ledger_txn_id, applied_at, executor_run_id)
			 VALUES ($1::uuid, $2::uuid, now(), 'cli-executor')`,
			appliedID, refLedgerID); err != nil {
			t.Fatalf("insert applied_proposals: %v", err)
		}
	}
	return cliUnmatchWorld{
		matchID:     matchID,
		ledgerTxnID: refLedgerID,
		proposalID:  proposalID,
	}
}

func seedCLIProposal(t *testing.T, d *testdb.DB, caseID, typ, status, key string) string {
	t.Helper()
	ctx := context.Background()
	var runID string
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO agent_runs (case_id, model, prompt_version, started_at)
		 VALUES ($1::uuid, 'scripted', 'prompt-v1', now())
		 RETURNING id::text`, caseID).Scan(&runID); err != nil {
		t.Fatalf("insert agent run: %v", err)
	}
	var id string
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO proposals
		     (case_id, agent_run_id, type, payload, rationale, confidence,
		      status, version, state_hash, idempotency_key, created_at)
		 VALUES ($1::uuid, $2::uuid, $3, '{"marker":"cli"}'::jsonb,
		         'cli fixture', 0.9, $4, 0, $5, $6, now())
		 RETURNING id::text`,
		caseID, runID, typ, status, "hash-"+key, key).Scan(&id); err != nil {
		t.Fatalf("insert %s proposal: %v", typ, err)
	}
	return id
}

func TestMatchUnmatchCLIJSONSuccess(t *testing.T) {
	d := cliFixture(t)
	world := seedCLIUnmatchWorld(t, d, false)

	var out bytes.Buffer
	err := runMatchTo(context.Background(), []string{
		"unmatch",
		"--id", world.matchID,
		"--reason", "operator requested",
		"--actor", "human:alice",
		"--json",
	}, &out)
	if err != nil {
		t.Fatalf("recon match unmatch --json: %v", err)
	}
	if code := exitCode(err); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	var result struct {
		MatchID            string    `json:"match_id"`
		BreaksReopened     int       `json:"breaks_reopened"`
		CasesReopened      int       `json:"cases_reopened"`
		ProposalsCancelled int       `json:"proposals_cancelled"`
		UnmatchedAt        time.Time `json:"unmatched_at"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("decode JSON %s: %v", out.String(), err)
	}
	if result.MatchID != world.matchID || result.BreaksReopened != 2 ||
		result.CasesReopened != 2 || result.ProposalsCancelled != 1 {
		t.Fatalf("JSON result = %+v, want match %s with 2/2/1",
			result, world.matchID)
	}
	if result.UnmatchedAt.IsZero() || time.Since(result.UnmatchedAt) > 5*time.Minute {
		t.Fatalf("unmatched_at = %v, want the current clock", result.UnmatchedAt)
	}
	var object map[string]any
	if err := json.Unmarshal(out.Bytes(), &object); err != nil {
		t.Fatalf("decode JSON object: %v", err)
	}
	if len(object) != 5 {
		t.Fatalf("JSON keys = %v, want exactly 5", object)
	}
	for _, key := range []string{"match_id", "breaks_reopened", "cases_reopened",
		"proposals_cancelled", "unmatched_at"} {
		if _, ok := object[key]; !ok {
			t.Fatalf("JSON keys = %v, missing %s", object, key)
		}
	}

	assertCLIUnmatched(t, d, world, "human:alice", "operator requested")
}

func TestMatchUnmatchCLIHumanOutputAndReplayConflict(t *testing.T) {
	d := cliFixture(t)
	world := seedCLIUnmatchWorld(t, d, false)

	var out bytes.Buffer
	err := runMatchTo(context.Background(), []string{
		"unmatch",
		"--id", world.matchID,
		"--reason", "operator requested",
		"--actor", "human:alice",
	}, &out)
	if err != nil {
		t.Fatalf("recon match unmatch: %v", err)
	}
	text := out.String()
	for _, want := range []string{
		"match " + world.matchID + " unmatched at",
		"breaks reopened: 2",
		"cases reopened: 2",
		"proposals cancelled: 1",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("human output does not contain %q:\n%s", want, text)
		}
	}

	out.Reset()
	err = runMatchTo(context.Background(), []string{
		"unmatch",
		"--id", world.matchID,
		"--reason", "operator requested",
		"--actor", "human:alice",
	}, &out)
	if err == nil {
		t.Fatal("repeated recon match unmatch = nil, want a conflict")
	}
	if code := exitCode(err); code != 1 {
		t.Fatalf("replay exit code = %d, want 1", code)
	}
	if !strings.Contains(err.Error(), "not active") {
		t.Fatalf("replay error = %q, want the not-active conflict", err)
	}
}

func TestMatchUnmatchCLIRefusal(t *testing.T) {
	d := cliFixture(t)
	world := seedCLIUnmatchWorld(t, d, true)

	var out bytes.Buffer
	err := runMatchTo(context.Background(), []string{
		"unmatch",
		"--id", world.matchID,
		"--reason", "operator requested",
		"--actor", "human:alice",
	}, &out)
	if err == nil {
		t.Fatal("recon match unmatch = nil, want the applied-adjustment refusal")
	}
	if code := exitCode(err); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	message := err.Error()
	for _, want := range []string{
		world.ledgerTxnID,
		"recon reverse --txn " + world.ledgerTxnID,
		"future",
		"not implemented in this release",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("refusal message %q does not contain %q", message, want)
		}
	}

	var status string
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT status FROM matches WHERE id = $1::uuid`, world.matchID).
		Scan(&status); err != nil {
		t.Fatalf("read match status: %v", err)
	}
	if status != "active" {
		t.Fatalf("match status = %s, want active after refusal", status)
	}
	var audits, outbox int
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_log WHERE action = 'match.unmatch'`).Scan(&audits); err != nil {
		t.Fatalf("count match audits: %v", err)
	}
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM outbox`).Scan(&outbox); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	if audits != 0 || outbox != 0 {
		t.Fatalf("after refusal match.unmatch audits = %d, outbox = %d, want 0/0",
			audits, outbox)
	}
}

func assertCLIUnmatched(t *testing.T, d *testdb.DB, world cliUnmatchWorld,
	actor, reason string) {
	t.Helper()
	ctx := context.Background()
	var (
		status        string
		unmatchedBy   *string
		unmatchReason *string
	)
	if err := d.Pool.QueryRow(ctx,
		`SELECT status, unmatched_by, unmatch_reason
		   FROM matches WHERE id = $1::uuid`, world.matchID).
		Scan(&status, &unmatchedBy, &unmatchReason); err != nil {
		t.Fatalf("read match: %v", err)
	}
	if status != "unmatched" || unmatchedBy == nil || *unmatchedBy != actor ||
		unmatchReason == nil || *unmatchReason != reason {
		t.Fatalf("match = %s/%v/%v, want unmatched by %s with reason %s",
			status, unmatchedBy, unmatchReason, actor, reason)
	}
	var legs int
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM match_legs
		  WHERE match_id = $1::uuid AND status = 'unmatched'`,
		world.matchID).Scan(&legs); err != nil {
		t.Fatalf("count unmatched legs: %v", err)
	}
	if legs != 2 {
		t.Fatalf("trigger-synced unmatched legs = %d, want 2", legs)
	}
	var openBreaks int
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM breaks b
		   JOIN case_breaks cb ON cb.break_id = b.id
		  WHERE cb.case_id IN ($1::uuid, $2::uuid) AND b.status = 'open'`,
		m2.FeeVarianceCaseID, m2.ReferenceTypoCaseID).Scan(&openBreaks); err != nil {
		t.Fatalf("count open breaks: %v", err)
	}
	if openBreaks != 2 {
		t.Fatalf("open linked breaks = %d, want 2", openBreaks)
	}
	var openCases int
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM cases
		  WHERE id IN ($1::uuid, $2::uuid) AND status = 'open'`,
		m2.FeeVarianceCaseID, m2.ReferenceTypoCaseID).Scan(&openCases); err != nil {
		t.Fatalf("count open cases: %v", err)
	}
	if openCases != 2 {
		t.Fatalf("open cases = %d, want 2", openCases)
	}
	var proposalStatus string
	var proposalVersion int
	if err := d.Pool.QueryRow(ctx,
		`SELECT status, version FROM proposals WHERE id = $1::uuid`,
		world.proposalID).Scan(&proposalStatus, &proposalVersion); err != nil {
		t.Fatalf("read proposal: %v", err)
	}
	if proposalStatus != "superseded" || proposalVersion != 1 {
		t.Fatalf("proposal = %s/version %d, want superseded/1",
			proposalStatus, proposalVersion)
	}
	var topic, payload string
	if err := d.Pool.QueryRow(ctx,
		`SELECT topic, payload::text FROM outbox
		  WHERE aggregate_id = $1 ORDER BY created_at, id`, world.proposalID).
		Scan(&topic, &payload); err != nil {
		t.Fatalf("read cancel outbox row: %v", err)
	}
	if topic != "approval.cancel" || !strings.Contains(payload, "case_reopened") {
		t.Fatalf("cancel row = %s %s, want approval.cancel with case_reopened", topic, payload)
	}
	var matchAudits int
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log
		  WHERE action = 'match.unmatch' AND entity_id = $1`,
		world.matchID).Scan(&matchAudits); err != nil {
		t.Fatalf("count match audits: %v", err)
	}
	if matchAudits != 1 {
		t.Fatalf("match.unmatch audits = %d, want 1", matchAudits)
	}
}
