package audit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// chainLockKey is the global advisory lock that serializes chain appends. It is
// an arbitrary constant ("reconaud" in ASCII); the lock is transaction-scoped,
// so it is released by commit or rollback.
const chainLockKey int64 = 0x7265636f6e617564

// Append writes one audit entry into the caller's transaction and returns the
// committed-shape record. It never starts its own transaction: audit rows must
// roll back with the domain change they describe (FIN-30).
//
// Appending takes a global pg_advisory_xact_lock, reads the chain head, assigns
// the next contiguous seq, computes row_hash over canonical JSON, and inserts
// the audit_log row plus the audit_chain_head mirror in the same transaction.
// The spec's throughput tradeoff is documented in doc.go.
func Append(ctx context.Context, tx pgx.Tx, e Entry) (Record, error) {
	if err := e.validate(); err != nil {
		return Record{}, err
	}
	if tx == nil {
		return Record{}, ErrNoTransaction
	}

	before, err := normalizeJSON(e.Before)
	if err != nil {
		return Record{}, err
	}
	after, err := normalizeJSON(e.After)
	if err != nil {
		return Record{}, err
	}
	ts := e.Timestamp.UTC().Truncate(time.Microsecond)

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, chainLockKey); err != nil {
		return Record{}, fmt.Errorf("audit: acquire chain lock: %w", err)
	}

	var prevSeq int64
	var prevHash []byte
	err = tx.QueryRow(ctx,
		`SELECT seq, row_hash FROM audit_chain_head ORDER BY seq DESC LIMIT 1`).
		Scan(&prevSeq, &prevHash)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		prevSeq = 0
		prevHash = zeroPrevHash()
	case err != nil:
		return Record{}, fmt.Errorf("audit: read chain head: %w", err)
	}
	if len(prevHash) != 32 {
		return Record{}, fmt.Errorf("audit: chain head row_hash length = %d, want 32", len(prevHash))
	}
	seq := prevSeq + 1

	rowHash, err := computeRowHash(
		seq, ts, string(e.Actor.Type()), e.Actor.ID(), e.Action,
		e.EntityType, e.EntityID, before, after, e.TraceID, prevHash)
	if err != nil {
		return Record{}, err
	}

	beforeArg, err := payloadArg(before)
	if err != nil {
		return Record{}, err
	}
	afterArg, err := payloadArg(after)
	if err != nil {
		return Record{}, err
	}

	var id string
	if err := tx.QueryRow(ctx,
		`INSERT INTO audit_log
		     (seq, ts, actor_type, actor_id, action, entity_type, entity_id,
		      before, after, trace_id, prev_hash, row_hash)
		 OVERRIDING SYSTEM VALUE
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9::jsonb, $10, $11, $12)
		 RETURNING id::text`,
		seq, ts, string(e.Actor.Type()), e.Actor.ID(), e.Action, e.EntityType,
		e.EntityID, beforeArg, afterArg, e.TraceID, prevHash, rowHash).
		Scan(&id); err != nil {
		return Record{}, fmt.Errorf("audit: insert audit_log: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO audit_chain_head (seq, row_hash) VALUES ($1, $2)`,
		seq, rowHash); err != nil {
		return Record{}, fmt.Errorf("audit: insert audit_chain_head: %w", err)
	}

	return Record{
		ID:        id,
		Seq:       seq,
		Timestamp: ts,
		PrevHash:  bytes.Clone(prevHash),
		RowHash:   bytes.Clone(rowHash),
	}, nil
}

// payloadArg renders a normalized JSON payload for the JSONB insert. Nil is
// stored as SQL NULL; both sides of the hash canonicalize it to JSON null.
func payloadArg(payload any) (any, error) {
	if payload == nil {
		return nil, nil
	}
	canonical, err := canonicalJSON(payload)
	if err != nil {
		return nil, err
	}
	return string(canonical), nil
}
