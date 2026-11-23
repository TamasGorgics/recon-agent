package worker_test

import (
	"context"
	"errors"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/agent"
	"github.com/TamasGorgics/recon-agent/internal/cases"
	"github.com/TamasGorgics/recon-agent/internal/worker"
)

type recordingWorkerObserver struct {
	codes []string
}

func (o *recordingWorkerObserver) WorkerFailed(code string) { o.codes = append(o.codes, code) }

func mustObservedWorker(t *testing.T, casesSvc worker.CaseService, runner worker.Runner, observer worker.Observer) *worker.Worker {
	t.Helper()
	return mustWorker(t, worker.Options{
		Config:   defaultWorkerConfig(),
		Cases:    casesSvc,
		Runner:   runner,
		Switch:   &fakeSwitch{enabled: true},
		Spend:    &fakeSpend{},
		Observer: observer,
	})
}

// TestWorkerObserverRecordsUnexpectedRunFailure proves the finalize default
// branch emits one bounded code for an unclassified run error.
func TestWorkerObserverRecordsUnexpectedRunFailure(t *testing.T) {
	fc := newFakeCases("a")
	observer := &recordingWorkerObserver{}
	runner := &fakeRunner{fn: func(context.Context, string) (agent.Result, error) {
		return agent.Result{RunID: "run-a"}, errors.New("provider exploded: raw detail")
	}}
	w := mustObservedWorker(t, fc, runner, observer)

	if err := w.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("ProcessOnce: %v", err)
	}
	if len(observer.codes) != 1 || observer.codes[0] != worker.ErrorCodeInternal {
		t.Fatalf("observed codes = %v, want [internal]", observer.codes)
	}
}

// TestWorkerObserverSilentOnCleanOutcome proves a normal finalized run emits no
// worker error.
func TestWorkerObserverSilentOnCleanOutcome(t *testing.T) {
	fc := newFakeCases("a")
	observer := &recordingWorkerObserver{}
	w := mustObservedWorker(t, fc, &fakeRunner{}, observer)

	if err := w.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("ProcessOnce: %v", err)
	}
	if len(observer.codes) != 0 {
		t.Fatalf("observed codes = %v, want none", observer.codes)
	}
}

// TestWorkerObserverRecordsLeaseFailure proves an unexpected lease error is
// counted while the empty-queue sentinel is not.
func TestWorkerObserverRecordsLeaseFailure(t *testing.T) {
	fc := newFakeCases("a")
	fc.leaseErr = errors.New("db down")
	observer := &recordingWorkerObserver{}
	w := mustObservedWorker(t, fc, &fakeRunner{}, observer)

	if err := w.ProcessOnce(context.Background()); err == nil {
		t.Fatal("ProcessOnce succeeded, want lease error")
	}
	if len(observer.codes) != 1 || observer.codes[0] != worker.ErrorCodeInternal {
		t.Fatalf("observed codes = %v, want [internal]", observer.codes)
	}

	empty := newFakeCases()
	empty.leaseErr = cases.ErrNoCase
	observer = &recordingWorkerObserver{}
	w = mustObservedWorker(t, empty, &fakeRunner{}, observer)
	if err := w.ProcessOnce(context.Background()); !errors.Is(err, cases.ErrNoCase) {
		t.Fatalf("empty ProcessOnce error = %v, want ErrNoCase", err)
	}
	if len(observer.codes) != 0 {
		t.Fatalf("empty queue observed codes = %v, want none", observer.codes)
	}
}
