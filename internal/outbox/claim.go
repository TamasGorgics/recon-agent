package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/db"
)

// claimSQL selects due rows in deterministic order, moves their
// next_attempt_at to the lease deadline, and returns the original due order.
// due.next_attempt_at is carried through RETURNING so the outer ORDER BY keeps
// the CTE's deterministic order; UPDATE ... RETURNING alone does not promise
// one.
const claimSQL = `
WITH due AS (
    SELECT id, next_attempt_at
      FROM outbox
     WHERE delivered_at IS NULL
       AND dead = false
       AND next_attempt_at <= $1
     ORDER BY next_attempt_at, id
     FOR UPDATE SKIP LOCKED
     LIMIT $2
), claimed AS (
    UPDATE outbox o
       SET next_attempt_at = $3
      FROM due
     WHERE o.id = due.id
    RETURNING o.id::text AS id,
              o.topic,
              o.payload,
              o.idempotency_key,
              o.attempts,
              due.next_attempt_at AS due_at
)
SELECT id, topic, payload, idempotency_key, attempts
  FROM claimed
 ORDER BY due_at, id`

// claim runs one short transaction: select due rows with SKIP LOCKED, lease
// them, and commit before returning. No handler or network I/O runs while this
// transaction is open.
func (w *Worker) claim(ctx context.Context, now time.Time) ([]Message, error) {
	var messages []Message
	err := db.WithTx(ctx, w.pool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		// The db.WithTx retry policy may run this callback more than once.
		messages = nil

		rows, err := tx.Query(ctx, claimSQL, now, w.batchSize, now.Add(w.lease))
		if err != nil {
			return fmt.Errorf("query due messages: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var (
				msg     Message
				payload []byte
			)
			if err := rows.Scan(&msg.ID, &msg.Topic, &payload, &msg.IdempotencyKey, &msg.Attempts); err != nil {
				return fmt.Errorf("scan due message: %w", err)
			}
			msg.Payload = append(json.RawMessage(nil), payload...)
			messages = append(messages, msg)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read due messages: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("outbox: claim due messages: %w", err)
	}
	return messages, nil
}
