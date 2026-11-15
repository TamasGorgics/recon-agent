//go:build integration

// covers: FIN-22

package outbox_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/outbox"
)

// TestExecutorProposalAppliedInterop proves the M4-06 contract: the executor's
// direct proposal.applied outbox insert (explicit UUID, aggregate proposal,
// key proposal.applied:<proposal id>, DB timestamps) is claimed and delivered
// by an outbox worker with a registered handler, with one unique side effect.
// The row is inserted through the real recon_executor login to exercise the
// executor's grants and ON CONFLICT conflict-target privilege.
func TestExecutorProposalAppliedInterop(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	const (
		outboxID      = "11111111-1111-4111-8111-111111111111"
		proposalID    = "22222222-2222-4222-8222-222222222222"
		caseID        = "33333333-3333-4333-8333-333333333333"
		ledgerTxnID   = "44444444-4444-4444-8444-444444444444"
		executorRunID = "55555555-5555-4555-8555-555555555555"
		topic         = "proposal.applied"
	)
	key := topic + ":" + proposalID

	payload, err := json.Marshal(map[string]any{
		"proposal_id":     proposalID,
		"case_id":         caseID,
		"type":            "adjustment",
		"ledger_txn_id":   ledgerTxnID,
		"applied_at":      "2026-11-15T00:00:00Z",
		"executor_run_id": executorRunID,
	})
	if err != nil {
		t.Fatalf("marshal proposal.applied payload: %v", err)
	}

	// Exact M4-06 apply.go enqueueApplied shape: explicit UUID, aggregate
	// proposal/<proposal id>, CURRENT_TIMESTAMP due times for this test, and
	// ON CONFLICT without RETURNING.
	const insertSQL = `INSERT INTO outbox
	     (id, aggregate_type, aggregate_id, topic, payload, idempotency_key,
	      created_at, next_attempt_at)
	 VALUES ($1::uuid, 'proposal', $2, 'proposal.applied', $3::jsonb, $4,
	         CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
	 ON CONFLICT (idempotency_key) DO NOTHING`

	executorPool := openExecutorPool(t, w)
	if _, err := executorPool.Exec(ctx, insertSQL, outboxID, proposalID, string(payload), key); err != nil {
		t.Fatalf("executor insert proposal.applied: %v", err)
	}
	// The executor is idempotent on the outbox key; a second apply inserts
	// nothing.
	if _, err := executorPool.Exec(ctx, insertSQL, outboxID, proposalID, string(payload), key); err != nil {
		t.Fatalf("executor duplicate insert proposal.applied: %v", err)
	}
	if got := w.countByKey(t, key); got != 1 {
		t.Fatalf("outbox rows for %q = %d, want 1", key, got)
	}

	row := w.loadByKey(t, key)
	if row.ID != outboxID {
		t.Fatalf("outbox id = %s, want %s", row.ID, outboxID)
	}
	if row.AggregateType != "proposal" || row.AggregateID != proposalID {
		t.Fatalf("aggregate = %s/%s, want proposal/%s", row.AggregateType, row.AggregateID, proposalID)
	}
	if row.Topic != topic {
		t.Fatalf("topic = %s, want %s", row.Topic, topic)
	}
	if row.DeliveredAt != nil || row.Dead || row.Attempts != 0 {
		t.Fatalf("fresh row delivered/attempts/dead = %v/%d/%t, want nil/0/false",
			row.DeliveredAt, row.Attempts, row.Dead)
	}

	channel := outbox.NewFakeChannel()
	worker := w.newWorker(t, map[string]outbox.Handler{topic: channel}, nil)

	claimed, err := worker.Process(ctx)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if claimed != 1 {
		t.Fatalf("claimed = %d, want 1", claimed)
	}
	if got := channel.Delivered(key); got != 1 {
		t.Fatalf("unique fake side effects = %d, want 1", got)
	}
	messages := channel.Messages()
	if len(messages) != 1 {
		t.Fatalf("handler messages = %d, want 1", len(messages))
	}
	msg := messages[0]
	if msg.ID != outboxID || msg.Topic != topic || msg.IdempotencyKey != key || msg.Attempts != 0 {
		t.Fatalf("handler message = id %s topic %s key %s attempts %d, want %s/%s/%s/0",
			msg.ID, msg.Topic, msg.IdempotencyKey, msg.Attempts, outboxID, topic, key)
	}

	var gotPayload map[string]any
	if err := json.Unmarshal(msg.Payload, &gotPayload); err != nil {
		t.Fatalf("decode delivered payload: %v", err)
	}
	if gotPayload["proposal_id"] != proposalID {
		t.Fatalf("payload proposal_id = %v, want %s", gotPayload["proposal_id"], proposalID)
	}
	if gotPayload["case_id"] != caseID {
		t.Fatalf("payload case_id = %v, want %s", gotPayload["case_id"], caseID)
	}
	if gotPayload["type"] != "adjustment" {
		t.Fatalf("payload type = %v, want adjustment", gotPayload["type"])
	}
	if gotPayload["ledger_txn_id"] != ledgerTxnID {
		t.Fatalf("payload ledger_txn_id = %v, want %s", gotPayload["ledger_txn_id"], ledgerTxnID)
	}
	if gotPayload["executor_run_id"] != executorRunID {
		t.Fatalf("payload executor_run_id = %v, want %s", gotPayload["executor_run_id"], executorRunID)
	}

	row = w.load(t, outboxID)
	if row.DeliveredAt == nil {
		t.Fatal("delivered_at = NULL after delivery, want set")
	}
	if row.DeliveredAt.Before(w.baseTime) || row.DeliveredAt.After(w.clock.Now().Add(time.Second)) {
		t.Fatalf("delivered_at = %s, want within the worker clock window", row.DeliveredAt)
	}
	if row.Attempts != 0 || row.Dead {
		t.Fatalf("attempts/dead = %d/%t, want 0/false", row.Attempts, row.Dead)
	}

	// The delivered executor row is never delivered again.
	w.clock.Advance(time.Hour)
	claimed, err = worker.Process(ctx)
	if err != nil {
		t.Fatalf("second Process: %v", err)
	}
	if claimed != 0 {
		t.Fatalf("second claimed = %d, want 0", claimed)
	}
	if got := channel.Delivered(key); got != 1 {
		t.Fatalf("unique fake side effects after second poll = %d, want 1", got)
	}
}
