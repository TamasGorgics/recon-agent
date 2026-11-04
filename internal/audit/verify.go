package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Querier is satisfied by *pgxpool.Pool and pgx.Tx, so Verify can run inside a
// transaction (tests tamper and verify atomically) or on a read pool.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ProblemKind classifies a chain verification finding.
type ProblemKind string

const (
	// ProblemHashMismatch means a row's stored row_hash is not the hash of its
	// current column values.
	ProblemHashMismatch ProblemKind = "hash_mismatch"
	// ProblemBrokenLink means a row's prev_hash is not the previous row's
	// row_hash.
	ProblemBrokenLink ProblemKind = "broken_link"
	// ProblemGap means expected seq values are missing (at the start, middle,
	// or before fromSeq).
	ProblemGap ProblemKind = "gap"
	// ProblemTruncatedTail means audit_chain_head is ahead of the last row in
	// audit_log, so the tail was deleted.
	ProblemTruncatedTail ProblemKind = "truncated_tail"
	// ProblemHeadMismatch means the mirror head does not match the audit row it
	// mirrors, or audit rows exist beyond the head.
	ProblemHeadMismatch ProblemKind = "head_mismatch"
)

// Problem is one verification finding.
type Problem struct {
	Seq    int64
	Kind   ProblemKind
	Detail string
}

// String renders a finding for logs and test failures.
func (p Problem) String() string {
	return fmt.Sprintf("seq %d: %s (%s)", p.Seq, p.Kind, p.Detail)
}

// Report is the result of Verify. Problems carries tamper and gap findings;
// a non-nil error means verification itself could not run.
type Report struct {
	// FromSeq is the first seq requested (clamped to 1).
	FromSeq int64
	// Checked is the number of rows hashed during this call.
	Checked int
	// HeadSeq is the seq of the latest audit_chain_head row, 0 when empty.
	HeadSeq int64
	// Problems lists every finding in seq order.
	Problems []Problem
}

// OK reports whether the verified segment is intact.
func (r Report) OK() bool { return len(r.Problems) == 0 }

// Verify recomputes the hash chain from fromSeq to the chain head and reports
// tampering, broken links, and gaps. fromSeq <= 1 verifies the whole chain.
//
// A value of fromSeq one past the head (head.seq+1) is a valid empty
// incremental verification; a larger value returns ErrFromSeqBeyondTail.
// Truncated tails are reported even when no rows remain to check.
func Verify(ctx context.Context, q Querier, fromSeq int64) (Report, error) {
	if q == nil {
		return Report{}, errors.New("audit: Verify requires a querier")
	}
	if fromSeq < 1 {
		fromSeq = 1
	}
	report := Report{FromSeq: fromSeq}

	var headSeq int64
	var headHash []byte
	err := q.QueryRow(ctx,
		`SELECT seq, row_hash FROM audit_chain_head ORDER BY seq DESC LIMIT 1`).
		Scan(&headSeq, &headHash)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		headSeq = 0
		headHash = nil
	case err != nil:
		return Report{}, fmt.Errorf("audit: read chain head: %w", err)
	}
	report.HeadSeq = headSeq

	var minSeq, maxSeq int64
	if err := q.QueryRow(ctx,
		`SELECT COALESCE(MIN(seq), 0), COALESCE(MAX(seq), 0) FROM audit_log`).
		Scan(&minSeq, &maxSeq); err != nil {
		return Report{}, fmt.Errorf("audit: read audit_log bounds: %w", err)
	}

	// Tail detection: the chain alone cannot tell a truncated tail from a
	// complete chain, so the append-only mirror head is authoritative.
	switch {
	case headSeq > maxSeq:
		report.Problems = append(report.Problems, Problem{
			Seq:    maxSeq + 1,
			Kind:   ProblemTruncatedTail,
			Detail: fmt.Sprintf("chain head is seq %d but audit_log ends at seq %d", headSeq, maxSeq),
		})
	case headSeq < maxSeq:
		report.Problems = append(report.Problems, Problem{
			Seq:    headSeq + 1,
			Kind:   ProblemHeadMismatch,
			Detail: fmt.Sprintf("audit_log has rows through seq %d but chain head is seq %d", maxSeq, headSeq),
		})
	case headSeq > 0:
		var mirrored []byte
		if err := q.QueryRow(ctx, `SELECT row_hash FROM audit_log WHERE seq = $1`, headSeq).
			Scan(&mirrored); err != nil {
			return Report{}, fmt.Errorf("audit: read head row: %w", err)
		}
		if !bytes.Equal(mirrored, headHash) {
			report.Problems = append(report.Problems, Problem{
				Seq:    headSeq,
				Kind:   ProblemHeadMismatch,
				Detail: "audit_chain_head.row_hash does not match the audit_log row it mirrors",
			})
		}
	}

	if fromSeq > headSeq+1 {
		return Report{}, fmt.Errorf("%w: fromSeq %d, head seq %d", ErrFromSeqBeyondTail, fromSeq, headSeq)
	}

	// Establish the expected link before the first checked row.
	var expectedPrev []byte
	var expectedSeq int64
	if fromSeq == 1 {
		expectedPrev = zeroPrevHash()
		expectedSeq = 1
	} else {
		var predSeq int64
		var predHash []byte
		err := q.QueryRow(ctx,
			`SELECT seq, row_hash FROM audit_log WHERE seq < $1 ORDER BY seq DESC LIMIT 1`, fromSeq).
			Scan(&predSeq, &predHash)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			report.Problems = append(report.Problems, Problem{
				Seq:    fromSeq,
				Kind:   ProblemGap,
				Detail: fmt.Sprintf("chain has no row before seq %d; the start of the chain is missing", fromSeq),
			})
		case err != nil:
			return Report{}, fmt.Errorf("audit: read predecessor row: %w", err)
		default:
			expectedPrev = predHash
			expectedSeq = predSeq + 1
		}
	}

	rows, err := q.Query(ctx,
		`SELECT seq, ts, actor_type, actor_id, action, entity_type, entity_id,
		        before::text, after::text, trace_id, prev_hash, row_hash
		   FROM audit_log WHERE seq >= $1 ORDER BY seq`, fromSeq)
	if err != nil {
		return Report{}, fmt.Errorf("audit: read audit rows: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			seq                                    int64
			ts                                     time.Time
			actorType, actorID, action, entityType string
			entityID                               string
			beforeText, afterText, traceText       pgtype.Text
			prevHash, rowHash                      []byte
		)
		if err := rows.Scan(&seq, &ts, &actorType, &actorID, &action,
			&entityType, &entityID, &beforeText, &afterText, &traceText,
			&prevHash, &rowHash); err != nil {
			return Report{}, fmt.Errorf("audit: scan audit row: %w", err)
		}
		report.Checked++

		if expectedSeq != 0 && seq != expectedSeq {
			report.Problems = append(report.Problems, Problem{
				Seq:    expectedSeq,
				Kind:   ProblemGap,
				Detail: fmt.Sprintf("expected seq %d, found seq %d", expectedSeq, seq),
			})
		}
		if expectedPrev != nil && !bytes.Equal(prevHash, expectedPrev) {
			report.Problems = append(report.Problems, Problem{
				Seq:    seq,
				Kind:   ProblemBrokenLink,
				Detail: "prev_hash does not match the previous row's row_hash",
			})
		}

		before, err := decodePayload(beforeText)
		if err != nil {
			return Report{}, fmt.Errorf("audit: decode before payload at seq %d: %w", seq, err)
		}
		after, err := decodePayload(afterText)
		if err != nil {
			return Report{}, fmt.Errorf("audit: decode after payload at seq %d: %w", seq, err)
		}
		computed, err := computeRowHash(seq, ts, actorType, actorID, action,
			entityType, entityID, before, after, tracePointer(traceText), prevHash)
		if err != nil {
			return Report{}, fmt.Errorf("audit: recompute row hash at seq %d: %w", seq, err)
		}
		if !bytes.Equal(computed, rowHash) {
			report.Problems = append(report.Problems, Problem{
				Seq:    seq,
				Kind:   ProblemHashMismatch,
				Detail: "stored row_hash does not match the row's current values",
			})
		}

		expectedPrev = rowHash
		expectedSeq = seq + 1
	}
	if err := rows.Err(); err != nil {
		return Report{}, fmt.Errorf("audit: iterate audit rows: %w", err)
	}

	return report, nil
}

func decodePayload(text pgtype.Text) (any, error) {
	if !text.Valid {
		return nil, nil
	}
	return normalizeJSON(json.RawMessage(text.String))
}

func tracePointer(text pgtype.Text) *string {
	if !text.Valid {
		return nil
	}
	value := text.String
	return &value
}
