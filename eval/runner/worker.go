package runner

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/shopspring/decimal"

	"github.com/TamasGorgics/recon-agent/eval/sandbox"
)

// costState tracks the persisted result-cost sum for one run. The scheduler and
// workers share it; values only increase.
type costState struct {
	mu    sync.Mutex
	spent decimal.Decimal
}

func newCostState(initial decimal.Decimal) *costState {
	return &costState{spent: initial}
}

func (c *costState) value() decimal.Decimal {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.spent
}

// raise records at least value. A concurrent query can observe a lower sum than
// another worker already recorded, so the maximum is retained.
func (c *costState) raise(value decimal.Decimal) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if value.GreaterThan(c.spent) {
		c.spent = value
	}
}

func (c *costState) atOrOver(cap decimal.Decimal) bool {
	if cap.IsZero() {
		return false
	}
	return c.value().GreaterThanOrEqual(cap)
}

// executePool runs pending cases through a bounded worker pool. On the first
// hard error it stops scheduling new cases and lets accepted in-flight cases
// finish. The returned aborted flag means the cost cap intentionally stopped
// scheduling.
func executePool(ctx context.Context, execution *runExecution, pending []sandbox.CaseRef) (int, bool, error) {
	workers := execution.opts.Concurrency
	if workers > len(pending) {
		workers = len(pending)
	}
	if workers < 1 {
		return 0, false, nil
	}

	jobs := make(chan sandbox.CaseRef)
	stop := make(chan struct{})
	var (
		stopOnce sync.Once
		firstErr error
		errMu    sync.Mutex
		executed atomic.Int64
		wg       sync.WaitGroup
	)
	recordErr := func(err error) {
		errMu.Lock()
		if firstErr == nil {
			firstErr = err
			stopOnce.Do(func() { close(stop) })
		}
		errMu.Unlock()
	}

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ref := range jobs {
				if err := execution.executeCase(ctx, ref); err != nil {
					recordErr(err)
					return
				}
				executed.Add(1)
			}
		}()
	}

	aborted := false
schedule:
	for _, ref := range pending {
		select {
		case <-ctx.Done():
			break schedule
		case <-stop:
			break schedule
		default:
		}
		if execution.costs.atOrOver(execution.opts.MaxCostUSD) {
			aborted = true
			break
		}
		select {
		case jobs <- ref:
		case <-stop:
			break schedule
		case <-ctx.Done():
			break schedule
		}
	}
	close(jobs)
	wg.Wait()

	errMu.Lock()
	err := firstErr
	errMu.Unlock()
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	return int(executed.Load()), aborted, err
}
