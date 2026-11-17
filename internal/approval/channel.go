package approval

import (
	"context"
	"fmt"
	"net/http"
	"sync"
)

// FakeChannel is an in-memory Channel for tests and dry runs. It records every
// request and cancel in call order and returns a deterministic external
// reference derived from the request's channel and proposal id. It performs no
// network I/O.
type FakeChannel struct {
	mu sync.Mutex

	requests []ApprovalRequest
	cancels  []string

	requestErr error
	cancelErr  error

	callback       CallbackDecision
	callbackErr    error
	callbackCalled int
}

var _ Channel = (*FakeChannel)(nil)

// NewFakeChannel returns an empty recording channel.
func NewFakeChannel() *FakeChannel { return &FakeChannel{} }

// RequestApproval records req and returns "fake:<channel>:<proposal_id>" as the
// external reference. A configured request error is returned after recording.
func (f *FakeChannel) RequestApproval(ctx context.Context, req ApprovalRequest) (string, error) {
	if f == nil {
		return "", fmt.Errorf("%w: nil fake channel", ErrInvalidRequest)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f.mu.Lock()
	f.requests = append(f.requests, cloneApprovalRequest(req))
	err := f.requestErr
	f.mu.Unlock()
	if err != nil {
		return "", err
	}
	return fakeExternalRef(req), nil
}

// HandleCallback records the call and returns the configured callback decision.
// With no configured decision it fails closed with ErrInvalidRequest.
func (f *FakeChannel) HandleCallback(ctx context.Context, req *http.Request) (CallbackDecision, error) {
	if f == nil {
		return CallbackDecision{}, fmt.Errorf("%w: nil fake channel", ErrInvalidRequest)
	}
	if err := ctx.Err(); err != nil {
		return CallbackDecision{}, err
	}
	f.mu.Lock()
	f.callbackCalled++
	callback := f.callback
	err := f.callbackErr
	if err == nil && callback.ProposalID == "" {
		err = fmt.Errorf("%w: fake callback is not configured", ErrInvalidRequest)
	}
	f.mu.Unlock()
	if err != nil {
		return CallbackDecision{}, err
	}
	return callback, nil
}

// Cancel records externalRef and returns the configured cancel error.
func (f *FakeChannel) Cancel(ctx context.Context, externalRef string) error {
	if f == nil {
		return fmt.Errorf("%w: nil fake channel", ErrInvalidRequest)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	f.cancels = append(f.cancels, externalRef)
	err := f.cancelErr
	f.mu.Unlock()
	return err
}

// Requests returns a copy of the recorded requests in call order.
func (f *FakeChannel) Requests() []ApprovalRequest {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ApprovalRequest, len(f.requests))
	for i, req := range f.requests {
		out[i] = cloneApprovalRequest(req)
	}
	return out
}

// Cancels returns a copy of the recorded external references in call order.
func (f *FakeChannel) Cancels() []string {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cancels...)
}

// CallbackCalls reports how many callback invocations were recorded.
func (f *FakeChannel) CallbackCalls() int {
	if f == nil {
		return 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.callbackCalled
}

// SetRequestError configures RequestApproval to return err after recording.
func (f *FakeChannel) SetRequestError(err error) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requestErr = err
}

// SetCancelError configures Cancel to return err after recording.
func (f *FakeChannel) SetCancelError(err error) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelErr = err
}

// SetCallback configures the callback outcome and error.
func (f *FakeChannel) SetCallback(callback CallbackDecision, err error) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callback = callback
	f.callbackErr = err
}

func fakeExternalRef(req ApprovalRequest) string {
	return "fake:" + req.Channel + ":" + req.ProposalID
}

func cloneApprovalRequest(req ApprovalRequest) ApprovalRequest {
	if req.Payload != nil {
		req.Payload = append([]byte(nil), req.Payload...)
	}
	return req
}
