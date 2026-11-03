package ledger

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// reverseLockClass is the first key of the two-int advisory lock used to
// serialize reversals of one transaction. Advisory locks need no UPDATE
// privilege, unlike SELECT ... FOR UPDATE, and ledger rows are intentionally
// not updatable by recon_app (FIN-11).
const reverseLockClass int32 = 0x72657672 // "revr"

// Reverse posts the opposing transaction for txnID and links it with
// reversal_of (FIN-11). A transaction-scoped advisory lock keyed on txnID
// serializes concurrent reversals: the second sees the committed reversal and
// returns ErrAlreadyReversed. Reversing a reversal is allowed because that
// targets a different transaction.
//
// The reversal reuses the original value date. Period selection then follows
// D-2: an original in a closed period produces a reversal in the current open
// period with adjusts_period_id set (FIN-56).
func (s *Service) Reverse(ctx context.Context, tx pgx.Tx, req ReverseRequest) (Result, error) {
	if tx == nil {
		return Result{}, ErrNoTransaction
	}
	if s == nil || s.Clock == nil {
		return Result{}, ErrNoClock
	}
	if !isUUID(req.TxnID) {
		return Result{}, fmt.Errorf("%w: transaction id %q is not a UUID", ErrInvalidRequest, req.TxnID)
	}
	if strings.TrimSpace(req.Reason) == "" {
		return Result{}, fmt.Errorf("%w: reversal reason is required", ErrInvalidRequest)
	}
	if strings.TrimSpace(req.IdempotencyKey) == "" {
		return Result{}, fmt.Errorf("%w: idempotency key is empty", ErrInvalidRequest)
	}
	if !req.Actor.Valid() {
		return Result{}, fmt.Errorf("%w: actor is required", ErrInvalidRequest)
	}

	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock($1, hashtext($2))`, reverseLockClass, req.TxnID); err != nil {
		return Result{}, fmt.Errorf("ledger: lock reversal target: %w", err)
	}

	var originalValueDate time.Time
	err := tx.QueryRow(ctx,
		`SELECT value_date FROM ledger_txns WHERE id = $1::uuid`, req.TxnID).
		Scan(&originalValueDate)
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, fmt.Errorf("%w: %s", ErrTxnNotFound, req.TxnID)
	}
	if err != nil {
		return Result{}, fmt.Errorf("ledger: lock original transaction: %w", err)
	}

	lines, err := originalLines(ctx, tx, req.TxnID)
	if err != nil {
		return Result{}, err
	}
	if len(lines) == 0 {
		return Result{}, fmt.Errorf("%w: transaction %s has no entries", ErrInvalidRequest, req.TxnID)
	}

	return s.Post(ctx, tx, PostRequest{
		IdempotencyKey: req.IdempotencyKey,
		Kind:           KindReversal,
		ValueDate:      originalValueDate,
		Description:    req.Reason,
		Lines:          lines,
		Actor:          req.Actor,
		ID:             req.ID,
		CreatedAt:      req.CreatedAt,
		BookedAt:       req.BookedAt,
		ReversalOf:     req.TxnID,
	})
}

// originalLines loads the original entries and flips each side.
func originalLines(ctx context.Context, tx pgx.Tx, txnID string) ([]Line, error) {
	rows, err := tx.Query(ctx,
		`SELECT account_id::text, direction, amount_minor, currency,
		        COALESCE(external_ref, ''), metadata
		   FROM ledger_entries
		  WHERE txn_id = $1::uuid
		  ORDER BY account_id, direction, amount_minor, currency, id`, txnID)
	if err != nil {
		return nil, fmt.Errorf("ledger: load original entries: %w", err)
	}
	defer rows.Close()

	var lines []Line
	for rows.Next() {
		var (
			line      Line
			direction string
		)
		if err := rows.Scan(&line.AccountID, &direction, &line.AmountMinor,
			&line.Currency, &line.ExternalRef, &line.Metadata); err != nil {
			return nil, fmt.Errorf("ledger: scan original entry: %w", err)
		}
		line.Direction = Direction(direction).opposite()
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: iterate original entries: %w", err)
	}
	return lines, nil
}
