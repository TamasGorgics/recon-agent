package llm

import (
	"context"
	"sync"
)

// ScriptedStep is one canned outcome. Check runs after the step is consumed and
// its error is returned to the caller.
type ScriptedStep struct {
	Response Response
	Err      error
	Check    func(Request) error
}

// Scripted is a deterministic Client that pops queued steps. It performs no
// network, clock, or sleep work and is safe for concurrent use.
type Scripted struct {
	mu       sync.Mutex
	steps    []ScriptedStep
	index    int
	requests []Request
}

// NewScripted returns a Scripted client backed by steps.
func NewScripted(steps ...ScriptedStep) *Scripted {
	return &Scripted{steps: append([]ScriptedStep(nil), steps...)}
}

// Complete consumes one step.
func (s *Scripted) Complete(_ context.Context, req Request) (Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, req)
	if s.index >= len(s.steps) {
		return Response{}, ErrScriptedExhausted
	}
	step := s.steps[s.index]
	s.index++
	if step.Check != nil {
		if err := step.Check(req); err != nil {
			return Response{}, err
		}
	}
	return step.Response, step.Err
}

// Calls returns the number of Complete invocations, including exhausted ones.
func (s *Scripted) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

// Remaining returns the number of unconsumed steps.
func (s *Scripted) Remaining() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.steps) - s.index
}

// Requests returns a copy of every request observed, in call order.
func (s *Scripted) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}
