package runner

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrRunLocked reports that another runner currently holds the advisory lock
// for the same eval_runs row. A concurrent resume fails fast with this error
// before it creates a sandbox or executes a scenario.
var ErrRunLocked = errors.New("eval/runner: eval run is already locked by another runner")

// runLock is a session-scoped PostgreSQL advisory lock held for the whole
// resume/execution lifetime. It must be acquired on a dedicated pooled
// connection because pg_advisory_lock is session scoped.
type runLock struct {
	conn     *pgxpool.Conn
	major    int32
	minor    int32
	released bool
}

func acquireRunLock(ctx context.Context, pool *pgxpool.Pool, runID string) (*runLock, error) {
	major, minor, err := advisoryLockKey(runID)
	if err != nil {
		return nil, err
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("eval/runner: acquire results connection for run lock: %w", err)
	}
	var acquired bool
	if err := conn.QueryRow(ctx,
		`SELECT pg_try_advisory_lock($1::int4, $2::int4)`, major, minor).Scan(&acquired); err != nil {
		conn.Release()
		return nil, fmt.Errorf("eval/runner: try advisory lock for run %s: %w", runID, err)
	}
	if !acquired {
		conn.Release()
		return nil, fmt.Errorf("%w: %s", ErrRunLocked, runID)
	}
	return &runLock{conn: conn, major: major, minor: minor}, nil
}

// release unlocks and returns the connection to the pool. If the unlock fails,
// the connection is closed instead of being reused while still holding the
// advisory lock.
func (l *runLock) release(ctx context.Context) error {
	if l == nil || l.conn == nil || l.released {
		return nil
	}
	l.released = true
	unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, err := l.conn.Exec(unlockCtx,
		`SELECT pg_advisory_unlock($1::int4, $2::int4)`, l.major, l.minor)
	if err != nil {
		if raw := l.conn.Hijack(); raw != nil {
			_ = raw.Close(context.WithoutCancel(ctx))
		}
		return fmt.Errorf("eval/runner: release run lock: %w", err)
	}
	l.conn.Release()
	return nil
}

// advisoryLockKey maps a UUID to the two-int4 advisory-lock key space. The
// mapping is stable across processes. A hash collision can only cause safe
// false contention, never lock two different runs as the same run.
func advisoryLockKey(runID string) (int32, int32, error) {
	clean := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(runID)), "-", "")
	raw, err := hex.DecodeString(clean)
	if err != nil || len(raw) != 16 {
		return 0, 0, fmt.Errorf("eval/runner: run id %q is not a UUID", runID)
	}
	return int32(binary.BigEndian.Uint32(raw[0:4])),
		int32(binary.BigEndian.Uint32(raw[4:8])), nil
}
