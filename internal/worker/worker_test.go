package worker_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"go.uber.org/goleak"

	"github.com/TamasGorgics/recon-agent/internal/agent"
	"github.com/TamasGorgics/recon-agent/internal/cases"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/worker"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func unitClock() *clock.Fake {
	return clock.NewFake(time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))
}

// ---------------------------------------------------------------- fake cases

type fakeCase struct {
	id       string
	status   cases.Status
	version  int
	attempts int
	leasedBy string
}

type fakeCases struct {
	mu           sync.Mutex
	order        []string
	states       map[string]*fakeCase
	leaseCalls   int
	releaseCalls int
	releaseTries int
	attemptCalls int
	transitions  []cases.TransitionRequest
	releaseErr   error
	attemptErr   error
	leaseSignal  chan struct{}
	leaseErr     error
}

func newFakeCases(ids ...string) *fakeCases {
	f := &fakeCases{states: make(map[string]*fakeCase, len(ids)), leaseSignal: make(chan struct{}, 64)}
	for _, id := range ids {
		f.order = append(f.order, id)
		f.states[id] = &fakeCase{id: id, status: cases.StatusOpen}
	}
	return f
}

func (f *fakeCases) addOpen(id string, attempts int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.order = append(f.order, id)
	f.states[id] = &fakeCase{id: id, status: cases.StatusOpen, attempts: attempts}
}

func (f *fakeCases) get(id string) fakeCase {
	f.mu.Lock()
	defer f.mu.Unlock()
	state := f.states[id]
	if state == nil {
		return fakeCase{}
	}
	return *state
}

func (f *fakeCases) counts() (lease, release, attempts int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.leaseCalls, f.releaseTries, f.attemptCalls
}

func (f *fakeCases) getTransitions() []cases.TransitionRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]cases.TransitionRequest(nil), f.transitions...)
}

func (f *fakeCases) Lease(_ context.Context, workerID string, _ time.Duration) (cases.Case, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leaseCalls++
	if f.leaseErr != nil {
		return cases.Case{}, f.leaseErr
	}
	for _, id := range f.order {
		state := f.states[id]
		if state.status != cases.StatusOpen || state.leasedBy != "" {
			continue
		}
		state.status = cases.StatusInAgent
		state.leasedBy = workerID
		state.version++
		select {
		case f.leaseSignal <- struct{}{}:
		default:
		}
		return cases.Case{
			ID:       state.id,
			Status:   state.status,
			Version:  state.version,
			Attempts: state.attempts,
			LeasedBy: &state.leasedBy,
		}, nil
	}
	return cases.Case{}, cases.ErrNoCase
}

func (f *fakeCases) Release(_ context.Context, caseID, workerID string) (cases.Case, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releaseTries++
	state := f.states[caseID]
	if state == nil {
		return cases.Case{}, cases.ErrCaseNotFound
	}
	if state.status != cases.StatusInAgent || state.leasedBy == "" {
		return cases.Case{}, cases.ErrNotLeased
	}
	if state.leasedBy != workerID {
		return cases.Case{}, cases.ErrNotLeaseOwner
	}
	if f.releaseErr != nil {
		return cases.Case{}, f.releaseErr
	}
	f.releaseCalls++
	state.status = cases.StatusOpen
	state.leasedBy = ""
	state.version++
	return cases.Case{ID: state.id, Status: state.status, Version: state.version, Attempts: state.attempts}, nil
}

func (f *fakeCases) RecordFailedAttempt(_ context.Context, caseID, workerID string, maxAttempts int) (cases.Case, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attemptCalls++
	state := f.states[caseID]
	if state == nil {
		return cases.Case{}, cases.ErrCaseNotFound
	}
	if state.status != cases.StatusInAgent || state.leasedBy == "" {
		return cases.Case{}, cases.ErrNotLeased
	}
	if state.leasedBy != workerID {
		return cases.Case{}, cases.ErrNotLeaseOwner
	}
	if f.attemptErr != nil {
		return cases.Case{}, f.attemptErr
	}
	state.attempts++
	state.version++
	state.leasedBy = ""
	if state.attempts >= maxAttempts {
		state.status = cases.StatusEscalated
	} else {
		state.status = cases.StatusOpen
	}
	return cases.Case{ID: state.id, Status: state.status, Version: state.version, Attempts: state.attempts}, nil
}

func (f *fakeCases) ReclaimExpired(context.Context) (int, error) { return 0, nil }

func (f *fakeCases) Transition(_ context.Context, req cases.TransitionRequest) (cases.Case, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.transitions = append(f.transitions, req)
	state := f.states[req.CaseID]
	if state == nil {
		return cases.Case{}, cases.ErrCaseNotFound
	}
	if state.version != req.ExpectedVersion {
		return cases.Case{}, cases.ErrStale
	}
	state.status = req.Status
	state.version++
	state.leasedBy = ""
	return cases.Case{ID: state.id, Status: state.status, Version: state.version}, nil
}

// ------------------------------------------------------------- fake support

type fakeSwitch struct {
	mu      sync.Mutex
	enabled bool
	err     error
	calls   int
	called  chan int
}

func (s *fakeSwitch) AgentEnabled(context.Context) (bool, error) {
	s.mu.Lock()
	s.calls++
	n := s.calls
	enabled, err, called := s.enabled, s.err, s.called
	s.mu.Unlock()
	if called != nil {
		select {
		case called <- n:
		default:
		}
	}
	return enabled, err
}

func (s *fakeSwitch) setEnabled(enabled bool) {
	s.mu.Lock()
	s.enabled = enabled
	s.mu.Unlock()
}

func (s *fakeSwitch) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

type fakeSpend struct {
	mu    sync.Mutex
	total decimal.Decimal
	err   error
	since time.Time
}

func (s *fakeSpend) SpendSince(_ context.Context, since time.Time) (decimal.Decimal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.since = since
	return s.total, s.err
}

type fakeRunner struct {
	mu    sync.Mutex
	calls int
	ids   []string
	fn    func(ctx context.Context, caseID string) (agent.Result, error)
}

func (r *fakeRunner) Run(ctx context.Context, caseID string) (agent.Result, error) {
	r.mu.Lock()
	r.calls++
	r.ids = append(r.ids, caseID)
	fn := r.fn
	r.mu.Unlock()
	if fn != nil {
		return fn(ctx, caseID)
	}
	return agent.Result{RunID: "run-" + caseID, Outcome: agent.OutcomeProposal}, nil
}

func (r *fakeRunner) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

type fakeSleeper struct {
	mu    sync.Mutex
	calls int
	after func()
}

func (s *fakeSleeper) Sleep(ctx context.Context, _ time.Duration) error {
	s.mu.Lock()
	s.calls++
	after := s.after
	s.mu.Unlock()
	if after != nil {
		after()
	}
	return ctx.Err()
}

func (s *fakeSleeper) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// blockingSleeper mirrors the production timer: Sleep returns only when ctx is
// canceled, so the poll loop does not spin in tests.
type blockingSleeper struct{}

func (blockingSleeper) Sleep(ctx context.Context, _ time.Duration) error {
	<-ctx.Done()
	return ctx.Err()
}

func mustWorker(t *testing.T, opts worker.Options) *worker.Worker {
	t.Helper()
	if opts.ID == "" {
		opts.ID = "worker:test:1:abcdef01"
	}
	if opts.Clock == nil {
		opts.Clock = unitClock()
	}
	if opts.Logger == nil {
		opts.Logger = quietLogger()
	}
	w, err := worker.New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return w
}

func defaultWorkerConfig() config.WorkerConfig { return config.DefaultWorkerConfig() }

func cancelAfter(cancel context.CancelFunc, n int) func() {
	var (
		mu    sync.Mutex
		count int
	)
	return func() {
		mu.Lock()
		count++
		hit := count >= n
		mu.Unlock()
		if hit {
			cancel()
		}
	}
}

// ------------------------------------------------------------------- tests

func TestNewRejectsMissingDependencies(t *testing.T) {
	base := worker.Options{
		ID:     "worker:test:1:abcdef01",
		Config: defaultWorkerConfig(),
		Cases:  newFakeCases(),
		Runner: &fakeRunner{},
		Switch: &fakeSwitch{enabled: true},
		Spend:  &fakeSpend{},
		Clock:  unitClock(),
	}

	missing := map[string]func(*worker.Options){
		"cases":  func(o *worker.Options) { o.Cases = nil },
		"runner": func(o *worker.Options) { o.Runner = nil },
		"switch": func(o *worker.Options) { o.Switch = nil },
		"spend":  func(o *worker.Options) { o.Spend = nil },
		"clock":  func(o *worker.Options) { o.Clock = nil },
		"id":     func(o *worker.Options) { o.ID = "" },
		"config": func(o *worker.Options) { o.Config.Concurrency = 0 },
	}
	for name, mutate := range missing {
		t.Run(name, func(t *testing.T) {
			opts := base
			mutate(&opts)
			if _, err := worker.New(opts); !errors.Is(err, worker.ErrInvalidOptions) {
				t.Fatalf("New error = %v, want ErrInvalidOptions", err)
			}
		})
	}
}

func TestNewIDFormat(t *testing.T) {
	id, err := worker.NewID()
	if err != nil {
		t.Fatalf("NewID: %v", err)
	}
	pattern := regexp.MustCompile(`^worker:[^:]+:\d+:[0-9a-f]{8}$`)
	if !pattern.MatchString(id) {
		t.Fatalf("NewID = %q, want worker:<host>:<pid>:<8 hex>", id)
	}
	other, err := worker.NewID()
	if err != nil {
		t.Fatalf("NewID second: %v", err)
	}
	if id == other {
		t.Fatalf("NewID returned the same id twice: %q", id)
	}
}

func TestRunPausesWhenSwitchOff(t *testing.T) {
	fc := newFakeCases("a", "b")
	ctx, cancel := context.WithCancel(context.Background())
	sleeper := &fakeSleeper{after: cancelAfter(cancel, 1)}
	w := mustWorker(t, worker.Options{
		Config:  defaultWorkerConfig(),
		Cases:   fc,
		Runner:  &fakeRunner{},
		Switch:  &fakeSwitch{enabled: false},
		Spend:   &fakeSpend{},
		Sleeper: sleeper,
	})
	if err := w.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if lease, _, _ := fc.counts(); lease != 0 {
		t.Fatalf("leases = %d, want 0", lease)
	}
	if sleeper.callCount() != 1 {
		t.Fatalf("sleep calls = %d, want 1", sleeper.callCount())
	}
}

func TestRunPausesWhenSwitchUnreadable(t *testing.T) {
	fc := newFakeCases("a")
	ctx, cancel := context.WithCancel(context.Background())
	w := mustWorker(t, worker.Options{
		Config:  defaultWorkerConfig(),
		Cases:   fc,
		Runner:  &fakeRunner{},
		Switch:  &fakeSwitch{enabled: true, err: errors.New("db down")},
		Spend:   &fakeSpend{},
		Sleeper: &fakeSleeper{after: cancelAfter(cancel, 1)},
	})
	if err := w.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if lease, _, _ := fc.counts(); lease != 0 {
		t.Fatalf("leases = %d, want 0", lease)
	}
}

func TestRunPausesAtDailyBudget(t *testing.T) {
	fc := newFakeCases("a", "b")
	ctx, cancel := context.WithCancel(context.Background())
	spend := &fakeSpend{total: decimal.RequireFromString("5.00")}
	w := mustWorker(t, worker.Options{
		Config:  defaultWorkerConfig(),
		Cases:   fc,
		Runner:  &fakeRunner{},
		Switch:  &fakeSwitch{enabled: true},
		Spend:   spend,
		Sleeper: &fakeSleeper{after: cancelAfter(cancel, 1)},
	})
	if err := w.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if lease, _, _ := fc.counts(); lease != 0 {
		t.Fatalf("leases = %d, want 0", lease)
	}
	today := time.Date(2026, 2, 3, 0, 0, 0, 0, time.UTC)
	if !spend.since.Equal(today) {
		t.Fatalf("spend since = %s, want %s", spend.since, today)
	}
}

func TestRunConcurrencyCap(t *testing.T) {
	fc := newFakeCases("a", "b", "c", "d", "e", "f")
	started := make(chan string, len(fc.order))
	gate := make(chan struct{})

	var (
		activeMu sync.Mutex
		active   int
		maxSeen  int
	)
	runner := &fakeRunner{fn: func(ctx context.Context, caseID string) (agent.Result, error) {
		activeMu.Lock()
		active++
		if active > maxSeen {
			maxSeen = active
		}
		activeMu.Unlock()
		defer func() {
			activeMu.Lock()
			active--
			activeMu.Unlock()
		}()
		started <- caseID
		<-gate
		return agent.Result{RunID: "run-" + caseID, Outcome: agent.OutcomeProposal}, nil
	}}
	cfg := defaultWorkerConfig()
	cfg.Concurrency = 2
	ctx, cancel := context.WithCancel(context.Background())
	w := mustWorker(t, worker.Options{
		Config: cfg, Cases: fc, Runner: runner,
		Switch: &fakeSwitch{enabled: true}, Spend: &fakeSpend{},
	})
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for two concurrent runs")
		}
	}
	if lease, _, _ := fc.counts(); lease != 2 {
		t.Fatalf("leases while two runs are in flight = %d, want 2", lease)
	}

	cancel()
	close(gate)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
	activeMu.Lock()
	peak := maxSeen
	activeMu.Unlock()
	if peak > 2 {
		t.Fatalf("peak concurrency = %d, want <= 2", peak)
	}
}

func TestRunSwitchOffDuringSemaphoreWait(t *testing.T) {
	fc := newFakeCases("a", "b")
	started := make(chan string, 1)
	gate := make(chan struct{})
	runner := &fakeRunner{fn: func(_ context.Context, caseID string) (agent.Result, error) {
		started <- caseID
		<-gate
		return agent.Result{RunID: "run-" + caseID, Outcome: agent.OutcomeProposal}, nil
	}}
	sw := &fakeSwitch{enabled: true, called: make(chan int, 16)}
	cfg := defaultWorkerConfig()
	cfg.Concurrency = 1
	ctx, cancel := context.WithCancel(context.Background())
	w := mustWorker(t, worker.Options{
		Config: cfg, Cases: fc, Runner: runner, Switch: sw, Spend: &fakeSpend{},
		Sleeper: &fakeSleeper{after: cancelAfter(cancel, 1)},
	})
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first run never started")
	}

	// The second AgentEnabled call belongs to the poll that is about to block
	// on the full semaphore. Once it has run, flip the switch before freeing
	// the slot so the re-check is the only thing that can stop the lease.
	deadline := time.After(5 * time.Second)
	for calls := 0; calls < 2; {
		select {
		case calls = <-sw.called:
		case <-deadline:
			t.Fatalf("switch reads = %d, want at least 2", sw.callCount())
		}
	}
	sw.setEnabled(false)
	close(gate)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
	if lease, _, _ := fc.counts(); lease != 1 {
		t.Fatalf("leases = %d, want 1 (no lease after the switch flip)", lease)
	}
	if runner.callCount() != 1 {
		t.Fatalf("runs = %d, want 1 (no run after the switch flip)", runner.callCount())
	}
	if state := fc.get("b"); state.status == cases.StatusInAgent || state.leasedBy != "" {
		t.Fatalf("second case = %+v, want it left unleased", state)
	}
}

func TestProcessOnceAttemptCapEscalatesWithoutRun(t *testing.T) {
	fc := newFakeCases()
	fc.addOpen("poison", 3)
	runner := &fakeRunner{}
	w := mustWorker(t, worker.Options{
		Config: defaultWorkerConfig(), Cases: fc, Runner: runner,
		Switch: &fakeSwitch{enabled: true}, Spend: &fakeSpend{},
	})
	if err := w.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("ProcessOnce: %v", err)
	}
	if runner.callCount() != 0 {
		t.Fatalf("model runs = %d, want 0", runner.callCount())
	}
	transitions := fc.getTransitions()
	if len(transitions) != 1 {
		t.Fatalf("transitions = %d, want 1", len(transitions))
	}
	if transitions[0].Status != cases.StatusEscalated || transitions[0].EscalationReason != "agent_failed" {
		t.Fatalf("transition = %+v, want escalated/agent_failed", transitions[0])
	}
	if state := fc.get("poison"); state.status != cases.StatusEscalated {
		t.Fatalf("case status = %s, want escalated", state.status)
	}
}

func TestProcessOnceFinalizePolicy(t *testing.T) {
	tests := []struct {
		name        string
		result      agent.Result
		runErr      error
		wantRelease int
		wantAttempt int
	}{
		{"proposal", agent.Result{Outcome: agent.OutcomeProposal}, nil, 1, 0},
		{"escalated", agent.Result{Outcome: agent.OutcomeEscalated}, nil, 1, 0},
		{"budget exhausted", agent.Result{Outcome: agent.OutcomeBudgetExhausted}, nil, 0, 1},
		{"invalid output", agent.Result{Outcome: agent.OutcomeInvalidOutput}, nil, 0, 1},
		{"provider error", agent.Result{Outcome: agent.OutcomeProviderError}, nil, 1, 0},
		{"agent disabled", agent.Result{Outcome: agent.OutcomeEscalated}, agent.ErrAgentDisabled, 1, 0},
		{"canceled", agent.Result{}, context.Canceled, 1, 0},
		{"unexpected", agent.Result{}, errors.New("boom"), 1, 0},
		{"unknown outcome", agent.Result{Outcome: agent.Outcome("weird")}, nil, 1, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fc := newFakeCases("case-1")
			runner := &fakeRunner{fn: func(context.Context, string) (agent.Result, error) {
				return tc.result, tc.runErr
			}}
			w := mustWorker(t, worker.Options{
				Config: defaultWorkerConfig(), Cases: fc, Runner: runner,
				Switch: &fakeSwitch{enabled: true}, Spend: &fakeSpend{},
			})
			if err := w.ProcessOnce(context.Background()); err != nil {
				t.Fatalf("ProcessOnce: %v", err)
			}
			lease, release, attempts := fc.counts()
			if lease != 1 {
				t.Fatalf("leases = %d, want 1", lease)
			}
			if release != tc.wantRelease || attempts != tc.wantAttempt {
				t.Fatalf("release/attempt calls = %d/%d, want %d/%d",
					release, attempts, tc.wantRelease, tc.wantAttempt)
			}
			state := fc.get("case-1")
			if tc.wantAttempt == 1 {
				if state.attempts != 1 || state.status != cases.StatusOpen {
					t.Fatalf("counted failure = %s/%d, want open/1", state.status, state.attempts)
				}
			} else if state.attempts != 0 {
				t.Fatalf("attempts = %d, want 0", state.attempts)
			}
		})
	}
}

func TestProcessOnceLostLeaseNoRetry(t *testing.T) {
	fc := newFakeCases("case-1")
	fc.releaseErr = cases.ErrNotLeaseOwner
	runner := &fakeRunner{}
	w := mustWorker(t, worker.Options{
		Config: defaultWorkerConfig(), Cases: fc, Runner: runner,
		Switch: &fakeSwitch{enabled: true}, Spend: &fakeSpend{},
	})
	if err := w.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("ProcessOnce: %v", err)
	}
	if lease, _, _ := fc.counts(); lease != 1 {
		t.Fatalf("leases = %d, want 1", lease)
	}
	if fc.releaseTries != 1 {
		t.Fatalf("release attempts = %d, want 1 (no retry)", fc.releaseTries)
	}
}

func TestProcessOnceSwitchOffBeforeLease(t *testing.T) {
	fc := newFakeCases("case-1")
	w := mustWorker(t, worker.Options{
		Config: defaultWorkerConfig(), Cases: fc, Runner: &fakeRunner{},
		Switch: &fakeSwitch{enabled: false}, Spend: &fakeSpend{},
	})
	if err := w.ProcessOnce(context.Background()); !errors.Is(err, worker.ErrAgentDisabled) {
		t.Fatalf("ProcessOnce error = %v, want ErrAgentDisabled", err)
	}
	if lease, _, _ := fc.counts(); lease != 0 {
		t.Fatalf("leases = %d, want 0", lease)
	}
}

func TestProcessOnceDailyBudgetStopsLease(t *testing.T) {
	fc := newFakeCases("case-1")
	spend := &fakeSpend{total: decimal.RequireFromString("5.00")}
	w := mustWorker(t, worker.Options{
		Config: defaultWorkerConfig(), Cases: fc, Runner: &fakeRunner{},
		Switch: &fakeSwitch{enabled: true}, Spend: spend,
	})
	if err := w.ProcessOnce(context.Background()); !errors.Is(err, worker.ErrDailyBudgetReached) {
		t.Fatalf("ProcessOnce error = %v, want ErrDailyBudgetReached", err)
	}
	if lease, _, _ := fc.counts(); lease != 0 {
		t.Fatalf("leases = %d, want 0", lease)
	}
}

func TestProcessOnceLocalSpendStopsNextLease(t *testing.T) {
	fc := newFakeCases("case-1", "case-2")
	spend := &fakeSpend{total: decimal.Zero}
	runner := &fakeRunner{fn: func(_ context.Context, caseID string) (agent.Result, error) {
		return agent.Result{
			RunID: "run-" + caseID, Outcome: agent.OutcomeProposal,
			CostUSD: decimal.RequireFromString("6.00"),
		}, nil
	}}
	w := mustWorker(t, worker.Options{
		Config: defaultWorkerConfig(), Cases: fc, Runner: runner,
		Switch: &fakeSwitch{enabled: true}, Spend: spend,
	})
	if err := w.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("first ProcessOnce: %v", err)
	}
	if err := w.ProcessOnce(context.Background()); !errors.Is(err, worker.ErrDailyBudgetReached) {
		t.Fatalf("second ProcessOnce error = %v, want ErrDailyBudgetReached", err)
	}
	if lease, _, _ := fc.counts(); lease != 1 {
		t.Fatalf("leases = %d, want 1", lease)
	}
}

func TestProcessOnceSpendErrorDoesNotLease(t *testing.T) {
	fc := newFakeCases("case-1")
	w := mustWorker(t, worker.Options{
		Config: defaultWorkerConfig(), Cases: fc, Runner: &fakeRunner{},
		Switch: &fakeSwitch{enabled: true}, Spend: &fakeSpend{err: errors.New("db down")},
	})
	if err := w.ProcessOnce(context.Background()); err == nil {
		t.Fatal("ProcessOnce leased with an unreadable spend")
	}
	if lease, _, _ := fc.counts(); lease != 0 {
		t.Fatalf("leases = %d, want 0", lease)
	}
}

func TestRunGracefulShutdownReleasesInFlight(t *testing.T) {
	defer goleak.VerifyNone(t)
	fc := newFakeCases("case-1")
	started := make(chan struct{}, 1)
	runner := &fakeRunner{fn: func(ctx context.Context, caseID string) (agent.Result, error) {
		started <- struct{}{}
		<-ctx.Done()
		return agent.Result{RunID: "run-" + caseID}, ctx.Err()
	}}
	cfg := defaultWorkerConfig()
	cfg.ShutdownGrace = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	w := mustWorker(t, worker.Options{
		Config: cfg, Cases: fc, Runner: runner,
		Switch: &fakeSwitch{enabled: true}, Spend: &fakeSpend{},
		Sleeper: blockingSleeper{},
	})
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("run never started")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
	_, release, attempts := fc.counts()
	if release != 1 || attempts != 0 {
		t.Fatalf("release/attempt = %d/%d, want 1/0", release, attempts)
	}
	state := fc.get("case-1")
	if state.status != cases.StatusOpen || state.attempts != 0 || state.leasedBy != "" {
		t.Fatalf("case after shutdown = %+v, want open with no lease", state)
	}
}

func TestProcessOnceProviderErrorDoesNotIncrement(t *testing.T) {
	// Regression guard for the locked provider_error exception: a total outage
	// leaves cases queued and attempts unchanged.
	fc := newFakeCases("case-1", "case-2")
	runner := &fakeRunner{fn: func(context.Context, string) (agent.Result, error) {
		return agent.Result{Outcome: agent.OutcomeProviderError}, nil
	}}
	w := mustWorker(t, worker.Options{
		Config: defaultWorkerConfig(), Cases: fc, Runner: runner,
		Switch: &fakeSwitch{enabled: true}, Spend: &fakeSpend{},
	})
	for i := 0; i < 2; i++ {
		if err := w.ProcessOnce(context.Background()); err != nil {
			t.Fatalf("ProcessOnce %d: %v", i, err)
		}
	}
	if runner.callCount() != 2 {
		t.Fatalf("runs = %d, want 2 (one per case)", runner.callCount())
	}
	for _, id := range fc.order {
		if state := fc.get(id); state.attempts != 0 || state.status != cases.StatusOpen {
			t.Fatalf("case %s = %+v, want open/0", id, state)
		}
	}
}

func TestProcessOnceMaxAttemptsBelowCapStillRuns(t *testing.T) {
	fc := newFakeCases()
	fc.addOpen("almost-poison", 2)
	runner := &fakeRunner{}
	w := mustWorker(t, worker.Options{
		Config: defaultWorkerConfig(), Cases: fc, Runner: runner,
		Switch: &fakeSwitch{enabled: true}, Spend: &fakeSpend{},
	})
	if err := w.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("ProcessOnce: %v", err)
	}
	if runner.callCount() != 1 {
		t.Fatalf("model runs = %d, want 1", runner.callCount())
	}
	if got := len(fc.getTransitions()); got != 0 {
		t.Fatalf("transitions = %d, want 0", got)
	}
}

func TestRunLeaseErrorIsRetried(t *testing.T) {
	fc := newFakeCases("case-1")
	fc.leaseErr = errors.New("transient")
	ctx, cancel := context.WithCancel(context.Background())
	sleeper := &fakeSleeper{after: cancelAfter(cancel, 2)}
	w := mustWorker(t, worker.Options{
		Config: defaultWorkerConfig(), Cases: fc, Runner: &fakeRunner{},
		Switch: &fakeSwitch{enabled: true}, Spend: &fakeSpend{}, Sleeper: sleeper,
	})
	if err := w.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if lease, _, _ := fc.counts(); lease < 2 {
		t.Fatalf("lease attempts = %d, want at least 2", lease)
	}
	if sleeper.callCount() < 2 {
		t.Fatalf("sleep calls = %d, want at least 2", sleeper.callCount())
	}
}

func TestStartOfUTCDay(t *testing.T) {
	// The worker queries the UTC day boundary from the injected clock, not the
	// host time zone.
	fc := newFakeCases()
	spend := &fakeSpend{}
	ctx, cancel := context.WithCancel(context.Background())
	w := mustWorker(t, worker.Options{
		Config: defaultWorkerConfig(), Cases: fc, Runner: &fakeRunner{},
		Switch: &fakeSwitch{enabled: true}, Spend: spend,
		Clock:   clock.NewFake(time.Date(2026, 2, 3, 23, 59, 59, 0, time.UTC)),
		Sleeper: &fakeSleeper{after: cancelAfter(cancel, 1)},
	})
	_ = w.Run(ctx)
	want := time.Date(2026, 2, 3, 0, 0, 0, 0, time.UTC)
	if !spend.since.Equal(want) {
		t.Fatalf("spend since = %s, want %s", spend.since, want)
	}
}
