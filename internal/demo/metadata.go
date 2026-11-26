package demo

// M5-05 durable demo-owned row markers. The destructive reset guard can only
// allow rows that it can prove belong to a demo layer, so every row written by
// the M1, ops, or quality seeds is recorded in demo_metadata. The marker table
// lives in the public schema and is itself destroyed/recreated by demo.Reset.

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	demoMetadataTable = "demo_metadata"

	demoLayerM1      = "m1"
	demoLayerOps     = "ops"
	demoLayerQuality = "quality"
)

// demoQuerier is the read/write surface shared by *pgxpool.Pool and pgx.Tx, so
// every seed step can run either standalone or inside the one outer transaction.
type demoQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// demoTableKeys names a table whose rows are demo-owned and the SQL expression
// that produces the durable row key stored in demo_metadata.row_key.
type demoTableKeys struct {
	Table  string
	KeySQL string
}

// m1DemoTableKeys is every table the M1 pipeline can write. The snapshot taken
// before Run is diffed against these tables, so rows that pre-dated the run
// are never adopted as demo data.
var m1DemoTableKeys = []demoTableKeys{
	{"accounts", "id::text"},
	{"fee_schedules", "id::text"},
	{"reconciliation_scopes", "id::text"},
	{"ledger_txns", "id::text"},
	{"ledger_entries", "id::text"},
	{"audit_log", "id::text"},
	{"audit_chain_head", "seq::text"},
	{"settlement_batches", "id::text"},
	{"statement_balances", "id::text"},
	{"external_records", "id::text"},
	{"match_runs", "id::text"},
	{"matches", "id::text"},
	{"match_legs", "id::text"},
	{"breaks", "id::text"},
	{"cases", "id::text"},
	{"case_breaks", "case_id::text || '/' || break_id::text"},
}

// demoRowKeySQL is the union of every demo-written table key. A table absent
// from this map (and from the reference/infrastructure allowlists) must be
// empty before a reset is allowed.
var demoRowKeySQL = func() map[string]string {
	keys := map[string]string{}
	for _, spec := range m1DemoTableKeys {
		keys[spec.Table] = spec.KeySQL
	}
	keys["agent_runs"] = "id::text"
	keys["proposals"] = "id::text"
	keys["eval_runs"] = "id::text"
	keys["eval_case_results"] = "id::text"
	keys["approvers"] = "id::text"
	keys["approvals"] = "id::text"
	keys["applied_proposals"] = "proposal_id::text"
	return keys
}()

// newDemoBatchID returns a random RFC 4122 version 4 UUID used only to group
// marker rows from one seed invocation.
func newDemoBatchID() (string, error) {
	var raw [16]byte
	if _, err := io.ReadFull(rand.Reader, raw[:]); err != nil {
		return "", fmt.Errorf("demo metadata: generate batch id: %w", err)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x",
		raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16]), nil
}

// snapshotDemoRows captures the M1-owned row keys that already exist before a
// demo.Run. Only rows absent from this snapshot are marked as demo-owned, so a
// seed cannot adopt pre-existing untagged data.
func snapshotDemoRows(ctx context.Context, q demoQuerier) (map[string]map[string]struct{}, error) {
	snapshot := make(map[string]map[string]struct{}, len(m1DemoTableKeys))
	for _, spec := range m1DemoTableKeys {
		keys, err := loadDemoTableKeys(ctx, q, spec)
		if err != nil {
			return nil, err
		}
		snapshot[spec.Table] = keys
	}
	return snapshot, nil
}

type demoKeyQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func loadDemoTableKeys(ctx context.Context, q demoKeyQuerier, spec demoTableKeys) (map[string]struct{}, error) {
	rows, err := q.Query(ctx, "SELECT "+spec.KeySQL+" FROM "+pgx.Identifier{spec.Table}.Sanitize())
	if err != nil {
		return nil, fmt.Errorf("demo metadata: read keys from %s: %w", spec.Table, err)
	}
	defer rows.Close()

	keys := map[string]struct{}{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("demo metadata: scan key from %s: %w", spec.Table, err)
		}
		keys[key] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("demo metadata: iterate keys from %s: %w", spec.Table, err)
	}
	return keys, nil
}

// markNewDemoRows records every M1 table row that did not exist before the run.
// It runs inside the seed transaction, so either the rows and their markers
// commit together or neither does. The seed already holds EXCLUSIVE locks on
// every table it writes, so rows committed by other sessions can neither appear
// between the baseline snapshot and this diff nor be adopted as demo-owned.
func markNewDemoRows(ctx context.Context, q demoQuerier, batchID string, baseline map[string]map[string]struct{}) error {
	for _, spec := range m1DemoTableKeys {
		keys, err := loadDemoTableKeys(ctx, q, spec)
		if err != nil {
			return err
		}
		added := make([]string, 0, len(keys))
		for key := range keys {
			if _, existed := baseline[spec.Table][key]; !existed {
				added = append(added, key)
			}
		}
		if err := markDemoKeysTx(ctx, q, spec.Table, demoLayerM1, batchID, added); err != nil {
			return err
		}
	}
	return nil
}

// markDemoKeysTx inserts marker rows for explicit keys. Duplicate keys keep
// the original owning layer (a row can only be demo-owned once).
func markDemoKeysTx(ctx context.Context, tx demoQuerier, table, layer, batchID string, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO demo_metadata (table_name, row_key, layer, batch_id)
		 SELECT $1, key, $2, $3::uuid
		   FROM unnest($4::text[]) AS key
		 ON CONFLICT (table_name, row_key) DO NOTHING`,
		table, layer, batchID, keys); err != nil {
		return fmt.Errorf("demo metadata: mark %d %s rows: %w", len(keys), table, err)
	}
	return nil
}

// markDemoLedgerEntriesTx records the ledger_entries rows created by one ops
// plan. Entries are keyed by their own UUID, but the plan only knows the parent
// transaction IDs.
func markDemoLedgerEntriesTx(ctx context.Context, tx demoQuerier, layer, batchID string, txnIDs []string) error {
	if len(txnIDs) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO demo_metadata (table_name, row_key, layer, batch_id)
		 SELECT 'ledger_entries', id::text, $1, $2::uuid
		   FROM ledger_entries
		  WHERE txn_id = ANY($3::uuid[])
		 ON CONFLICT (table_name, row_key) DO NOTHING`,
		layer, batchID, txnIDs); err != nil {
		return fmt.Errorf("demo metadata: mark ledger entries: %w", err)
	}
	return nil
}

// markDemoAppliedProposalsTx records applied_proposals rows by proposal id.
func markDemoAppliedProposalsTx(ctx context.Context, tx demoQuerier, layer, batchID string, proposalIDs []string) error {
	if len(proposalIDs) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO demo_metadata (table_name, row_key, layer, batch_id)
		 SELECT 'applied_proposals', proposal_id::text, $1, $2::uuid
		   FROM applied_proposals
		  WHERE proposal_id = ANY($3::uuid[])
		 ON CONFLICT (table_name, row_key) DO NOTHING`,
		layer, batchID, proposalIDs); err != nil {
		return fmt.Errorf("demo metadata: mark applied proposals: %w", err)
	}
	return nil
}
