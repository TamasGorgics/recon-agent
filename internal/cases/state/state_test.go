package state

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const testCaseID = "11111111-1111-4111-8111-111111111111"

var testNow = time.Date(2026, 11, 18, 12, 30, 45, 123456000, time.UTC)

// legalEdge is the contract table used by the tests. It is intentionally
// independent of the production edge list so a missing or extra production
// row is caught.
type legalEdge struct {
	from       Status
	to         Status
	actor      string
	reason     string
	resolution string
}

var legalEdges = []legalEdge{
	{StatusOpen, StatusInAgent, "system:worker", "", ""},
	{StatusOpen, StatusEscalated, "system:case-lifecycle", "attempts_exhausted", ""},
	{StatusOpen, StatusResolved, "human:alice", "manual resolution", "manual"},

	{StatusInAgent, StatusOpen, "system:worker", "", ""},
	{StatusInAgent, StatusProposalPending, "system:agent-loop", "", ""},
	{StatusInAgent, StatusWaitingInfo, "system:agent-loop", "need a statement", ""},
	{StatusInAgent, StatusEscalated, "system:worker", "manual", ""},
	{StatusInAgent, StatusResolved, "system:matcher", "late match", "superseded_by_match"},

	{StatusProposalPending, StatusOpen, "system:case-lifecycle", "proposal_rejected", ""},
	{StatusProposalPending, StatusWaitingInfo, "system:agent-loop", "", ""},
	{StatusProposalPending, StatusEscalated, "system:case-lifecycle", "missing_settlement_sla", ""},
	{StatusProposalPending, StatusResolved, "human:alice", "manual resolution", "manual"},

	{StatusWaitingInfo, StatusOpen, "human:alice", "human_reply", ""},
	{StatusWaitingInfo, StatusEscalated, "system:case-lifecycle", "attempts_exhausted", ""},
	{StatusWaitingInfo, StatusResolved, "human:alice", "manual resolution", "manual"},

	{StatusEscalated, StatusOpen, "human:alice", "reopen after review", ""},
	{StatusEscalated, StatusResolved, "system:matcher", "late match", "superseded_by_match"},

	{StatusResolved, StatusOpen, "human:alice", "unmatch", ""},
}

func baseSnapshot(status Status, version int) Snapshot {
	return Snapshot{
		CaseID:   testCaseID,
		Status:   status,
		Version:  version,
		Attempts: 2,
	}
}

func request(to Status, actor, reason, resolution string) TransitionRequest {
	return TransitionRequest{
		CaseID:          testCaseID,
		To:              to,
		ExpectedVersion: 3,
		Actor:           actor,
		Reason:          reason,
		ResolutionType:  resolution,
		TraceID:         "trace-1",
		Now:             testNow,
	}
}

func decideRequest(t *testing.T, before Snapshot, req TransitionRequest) (plan, error) {
	t.Helper()
	parsed, err := parseRequest(req)
	if err != nil {
		return plan{}, err
	}
	return decide(before, parsed)
}

func requireIs(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func requireMessage(t *testing.T, err error, contains string) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want one containing %q", contains)
	}
	if !strings.Contains(err.Error(), contains) {
		t.Fatalf("error = %q, want it to contain %q", err, contains)
	}
}

// TestStatusSet pins the closed six-status set.
func TestStatusSet(t *testing.T) {
	want := []Status{"open", "in_agent", "proposal_pending", "waiting_info", "escalated", "resolved"}
	got := AllStatuses()
	if len(got) != len(want) {
		t.Fatalf("AllStatuses() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("AllStatuses()[%d] = %q, want %q", i, got[i], want[i])
		}
		if !got[i].Valid() {
			t.Fatalf("%q.Valid() = false", got[i])
		}
	}
	for _, invalid := range []Status{"", "OPEN", "escalated ", "bogus", "agent_failed"} {
		if invalid.Valid() {
			t.Fatalf("%q.Valid() = true", invalid)
		}
	}
	// AllStatuses returns a copy.
	got[0] = "mutated"
	if AllStatuses()[0] != StatusOpen {
		t.Fatal("AllStatuses() exposed the package slice")
	}
}

// TestEdgeMatrix walks the full 6x6 matrix. The 18 legal edges succeed with a
// valid actor/reason/resolution; the 12 illegal non-self pairs are
// ErrIllegalTransition (or ErrTerminalState from resolved); the 6 self pairs
// are ErrAlreadyInState.
func TestEdgeMatrix(t *testing.T) {
	legal := make(map[[2]Status]bool, len(legalEdges))
	for _, edge := range legalEdges {
		legal[[2]Status{edge.from, edge.to}] = true
	}
	if len(legal) != 18 {
		t.Fatalf("test table has %d legal edges, want 18", len(legal))
	}

	for _, from := range AllStatuses() {
		for _, to := range AllStatuses() {
			from, to := from, to
			t.Run(string(from)+"_to_"+string(to), func(t *testing.T) {
				before := baseSnapshot(from, 3)
				if from == to {
					_, err := decideRequest(t, before,
						request(to, "human:alice", "ignored", resolutionFor(to)))
					requireIs(t, err, ErrAlreadyInState)
					return
				}
				if legal[[2]Status{from, to}] {
					var spec legalEdge
					for _, edge := range legalEdges {
						if edge.from == from && edge.to == to {
							spec = edge
							break
						}
					}
					change, err := decideRequest(t, before,
						request(to, spec.actor, spec.reason, spec.resolution))
					if err != nil {
						t.Fatalf("legal %s -> %s: %v", from, to, err)
					}
					if change.reason != strings.TrimSpace(spec.reason) {
						t.Fatalf("reason = %q, want %q", change.reason, spec.reason)
					}
					wantResolution := ""
					if to == StatusResolved {
						wantResolution = spec.resolution
					}
					if change.resolution != wantResolution {
						t.Fatalf("resolution = %q, want %q", change.resolution, wantResolution)
					}
					return
				}

				// Every remaining non-self pair is absent from the table. Use a
				// request shape that would be valid for the target so the edge
				// (not a later field rule) is what fails.
				_, err := decideRequest(t, before,
					request(to, "human:alice", "reason", resolutionFor(to)))
				if from == StatusResolved {
					requireIs(t, err, ErrTerminalState)
					requireIs(t, err, ErrIllegalTransition)
					return
				}
				requireIs(t, err, ErrIllegalTransition)
			})
		}
	}
}

func resolutionFor(to Status) string {
	if to == StatusResolved {
		return "manual"
	}
	return ""
}

// TestActorMatrix covers the actor classes, agent rejection, malformed actors,
// and human superseded_by_match rejection.
func TestActorMatrix(t *testing.T) {
	tests := []struct {
		name       string
		from       Status
		to         Status
		actor      string
		reason     string
		resolution string
		want       error
	}{
		{"human cannot take worker edge", StatusOpen, StatusInAgent,
			"human:alice", "", "", ErrActorNotAllowed},
		{"agent cannot take worker edge", StatusOpen, StatusInAgent,
			"agent:model", "", "", ErrActorNotAllowed},
		{"system cannot reopen resolved", StatusResolved, StatusOpen,
			"system:case-lifecycle", "reopen", "", ErrActorNotAllowed},
		{"human reopens resolved", StatusResolved, StatusOpen,
			"human:alice", "reopen", "", nil},
		{"human replies to waiting_info", StatusWaitingInfo, StatusOpen,
			"human:alice", "human_reply", "", nil},
		{"system resumes waiting_info", StatusWaitingInfo, StatusOpen,
			"system:ingest", "new_ingest", "", nil},
		{"human may resolve", StatusOpen, StatusResolved,
			"human:alice", "manual resolution", "manual", nil},
		{"human cannot claim superseded_by_match", StatusOpen, StatusResolved,
			"human:alice", "manual resolution", "superseded_by_match", ErrActorNotAllowed},
		{"human superseded_by_match precedes missing reason", StatusOpen, StatusResolved,
			"human:alice", "", "superseded_by_match", ErrActorNotAllowed},
		{"system may use superseded_by_match", StatusOpen, StatusResolved,
			"system:matcher", "late match", "superseded_by_match", nil},
		{"system may use another resolution shape", StatusOpen, StatusResolved,
			"system:executor", "executor closure", "manual", nil},
		{"agent actor on legal system edge", StatusOpen, StatusEscalated,
			"agent:model", "manual", "", ErrActorNotAllowed},
		{"agent actor on illegal edge reports the edge", StatusOpen, StatusWaitingInfo,
			"agent:model", "", "", ErrIllegalTransition},
		{"actor suffix boundary", StatusOpen, StatusInAgent,
			"system:" + strings.Repeat("a", 128), "", "", nil},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, err := decideRequest(t, baseSnapshot(tc.from, 3),
				request(tc.to, tc.actor, tc.reason, tc.resolution))
			if tc.want == nil {
				if err != nil {
					t.Fatalf("error = %v, want nil", err)
				}
				return
			}
			requireIs(t, err, tc.want)
		})
	}

	malformed := []string{
		"",
		"alice",
		"Human:alice",
		"HUMAN:alice",
		"human:",
		"human: alice",
		"human:al ice",
		"human:ali\tce",
		"human:ali\nce",
		"human:ali\rce",
		"human:ali\x00ce",
		"human:ali\x7fce",
		"system:",
		"agent:",
		"robot:alice",
		"human:" + strings.Repeat("a", 129),
		"human:\xff",
	}
	for _, actor := range malformed {
		actor := actor
		t.Run("malformed_"+safeName(actor), func(t *testing.T) {
			_, err := decideRequest(t, baseSnapshot(StatusOpen, 3),
				request(StatusInAgent, actor, "", ""))
			requireIs(t, err, ErrInvalidRequest)
		})
	}
}

func safeName(value string) string {
	if value == "" {
		return "empty"
	}
	replacer := strings.NewReplacer("\t", "tab", "\n", "nl", "\r", "cr", "\x00", "nul",
		"\x7f", "del", "\xff", "ff", " ", "space")
	return replacer.Replace(value)
}

// TestReasonMatrix covers required/optional free text, free-text escalation
// reasons (the M4-10 deviation from the spec's closed set), and the 500-byte
// bound.
func TestReasonMatrix(t *testing.T) {
	long := strings.Repeat("a", 500)
	tooLong := strings.Repeat("a", 501)
	tests := []struct {
		name       string
		from       Status
		to         Status
		actor      string
		reason     string
		resolution string
		want       error
	}{
		{"open escalated needs a reason", StatusOpen, StatusEscalated,
			"system:case-lifecycle", "", "", ErrReasonRequired},
		{"open escalated blank reason", StatusOpen, StatusEscalated,
			"system:case-lifecycle", "   \t ", "", ErrReasonRequired},
		{"open escalated accepts a free-text agent reason", StatusOpen, StatusEscalated,
			"system:case-lifecycle", "injection_flagged", "", nil},
		{"open escalated accepts a free-text reason with spaces", StatusOpen, StatusEscalated,
			"system:agent-loop", "ambiguous evidence", "", nil},
		{"open escalated rejects 501 bytes", StatusOpen, StatusEscalated,
			"system:case-lifecycle", tooLong, "", ErrInvalidReason},
		{"open escalated rejects a control character", StatusOpen, StatusEscalated,
			"system:case-lifecycle", "bad\x00reason", "", ErrInvalidReason},
		{"open escalated accepts attempts_exhausted", StatusOpen, StatusEscalated,
			"system:case-lifecycle", "attempts_exhausted", "", nil},
		{"open escalated accepts missing_settlement_sla", StatusOpen, StatusEscalated,
			"system:case-lifecycle", "missing_settlement_sla", "", nil},
		{"open escalated accepts manual", StatusOpen, StatusEscalated,
			"system:case-lifecycle", "manual", "", nil},
		{"system release reason is optional", StatusInAgent, StatusOpen,
			"system:worker", "", "", nil},
		{"system release present reason is validated", StatusInAgent, StatusOpen,
			"system:worker", tooLong, "", ErrInvalidReason},
		{"human release needs a reason", StatusInAgent, StatusOpen,
			"human:operator", "", "", ErrReasonRequired},
		{"human release accepts 500 bytes", StatusInAgent, StatusOpen,
			"human:operator", long, "", nil},
		{"human release rejects 501 bytes", StatusInAgent, StatusOpen,
			"human:operator", tooLong, "", ErrInvalidReason},
		{"human release accepts newline and tab", StatusInAgent, StatusOpen,
			"human:operator", "line one\nline two\tend", "", nil},
		{"human release rejects NUL", StatusInAgent, StatusOpen,
			"human:operator", "bad\x00reason", "", ErrInvalidReason},
		{"human release rejects carriage return", StatusInAgent, StatusOpen,
			"human:operator", "bad\rreason", "", ErrInvalidReason},
		{"human release rejects DEL", StatusInAgent, StatusOpen,
			"human:operator", "bad\x7freason", "", ErrInvalidReason},
		{"human release rejects invalid UTF-8", StatusInAgent, StatusOpen,
			"human:operator", "bad\xffreason", "", ErrInvalidReason},
		{"system partial late match needs the exact reason", StatusEscalated, StatusOpen,
			"system:matcher", "", "", ErrReasonRequired},
		{"system partial late match rejects another reason", StatusEscalated, StatusOpen,
			"system:matcher", "reopened", "", ErrInvalidReason},
		{"system partial late match accepts its reason", StatusEscalated, StatusOpen,
			"system:matcher", "partial_late_match", "", nil},
		{"human escalated reopen needs a reason", StatusEscalated, StatusOpen,
			"human:operator", "", "", ErrReasonRequired},
		{"human escalated reopen accepts a reason", StatusEscalated, StatusOpen,
			"human:operator", "operator reviewed", "", nil},
		{"human waiting reply reason is optional", StatusWaitingInfo, StatusOpen,
			"human:alice", "", "", nil},
		{"human resolution reason is required", StatusOpen, StatusResolved,
			"human:alice", "", "manual", ErrReasonRequired},
		{"system resolution reason is optional", StatusOpen, StatusResolved,
			"system:matcher", "", "superseded_by_match", nil},
		{"reason is trimmed", StatusInAgent, StatusOpen,
			"human:operator", "  reviewed  ", "", nil},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			change, err := decideRequest(t, baseSnapshot(tc.from, 3),
				request(tc.to, tc.actor, tc.reason, tc.resolution))
			if tc.want != nil {
				requireIs(t, err, tc.want)
				return
			}
			if err != nil {
				t.Fatalf("error = %v, want nil", err)
			}
			if strings.TrimSpace(tc.reason) != change.reason {
				t.Fatalf("reason = %q, want trimmed %q", change.reason, tc.reason)
			}
		})
	}
}

// TestCanonicalEscalationReasons pins the exported lifecycle constants. The
// authority accepts escalation reasons as free text, but the canonical values
// are part of the lifecycle contract and must not drift.
func TestCanonicalEscalationReasons(t *testing.T) {
	want := map[string]string{
		EscalationAttemptsExhausted:    "attempts_exhausted",
		EscalationMissingSettlementSLA: "missing_settlement_sla",
		EscalationManual:               "manual",
	}
	for constant, value := range want {
		if constant != value {
			t.Fatalf("constant %q = %q, want %q", value, constant, value)
		}
	}
}

// TestResolutionMatrix covers required shape, trimming, the human reservation,
// and forbidden resolution on non-resolved targets.
func TestResolutionMatrix(t *testing.T) {
	valid64 := "a" + strings.Repeat("b", 63)
	tests := []struct {
		name       string
		from       Status
		to         Status
		actor      string
		reason     string
		resolution string
		want       error
	}{
		{"missing resolution", StatusOpen, StatusResolved,
			"human:alice", "manual", "", ErrResolutionRequired},
		{"blank resolution", StatusOpen, StatusResolved,
			"human:alice", "manual", "   ", ErrResolutionRequired},
		{"upper case resolution", StatusOpen, StatusResolved,
			"human:alice", "manual", "Manual", ErrInvalidRequest},
		{"punctuation resolution", StatusOpen, StatusResolved,
			"human:alice", "manual", "manual!", ErrInvalidRequest},
		{"leading digit resolution", StatusOpen, StatusResolved,
			"human:alice", "manual", "1manual", ErrInvalidRequest},
		{"space in resolution", StatusOpen, StatusResolved,
			"human:alice", "manual", "man ual", ErrInvalidRequest},
		{"hyphen in resolution", StatusOpen, StatusResolved,
			"human:alice", "manual", "manual-type", ErrInvalidRequest},
		{"65-byte resolution", StatusOpen, StatusResolved,
			"human:alice", "manual", strings.Repeat("a", 65), ErrInvalidRequest},
		{"single-letter resolution", StatusOpen, StatusResolved,
			"human:alice", "manual", "a", nil},
		{"64-byte resolution", StatusOpen, StatusResolved,
			"human:alice", "manual", valid64, nil},
		{"resolution is trimmed", StatusOpen, StatusResolved,
			"human:alice", "manual", "  manual  ", nil},
		{"human superseded_by_match", StatusOpen, StatusResolved,
			"human:alice", "manual", "  superseded_by_match  ", ErrActorNotAllowed},
		{"system superseded_by_match", StatusOpen, StatusResolved,
			"system:matcher", "late", "superseded_by_match", nil},
		{"resolution forbidden on in_agent", StatusOpen, StatusInAgent,
			"system:worker", "", "manual", ErrInvalidRequest},
		{"whitespace resolution forbidden on in_agent", StatusOpen, StatusInAgent,
			"system:worker", "", " ", ErrInvalidRequest},
		{"resolution forbidden on reopen", StatusResolved, StatusOpen,
			"human:alice", "reopen", "manual", ErrInvalidRequest},
		{"resolution required on waiting_info resolved", StatusWaitingInfo, StatusResolved,
			"human:alice", "manual", "", ErrResolutionRequired},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			change, err := decideRequest(t, baseSnapshot(tc.from, 3),
				request(tc.to, tc.actor, tc.reason, tc.resolution))
			if tc.want != nil {
				requireIs(t, err, tc.want)
				return
			}
			if err != nil {
				t.Fatalf("error = %v, want nil", err)
			}
			want := strings.TrimSpace(tc.resolution)
			if tc.to != StatusResolved {
				want = ""
			}
			if change.resolution != want {
				t.Fatalf("resolution = %q, want %q", change.resolution, want)
			}
		})
	}
}

// TestErrorPrecedence pins the deterministic validation order where errors can
// overlap: request shape, stored status, self, stale, terminal/edge, actor,
// reason, resolution.
func TestErrorPrecedence(t *testing.T) {
	validReq := request(StatusInAgent, "system:worker", "", "")

	t.Run("case id before target", func(t *testing.T) {
		req := validReq
		req.CaseID = "not-a-uuid"
		req.To = "bogus"
		req.ExpectedVersion = -1
		_, err := decideRequest(t, baseSnapshot(StatusOpen, 3), req)
		requireIs(t, err, ErrInvalidRequest)
		requireMessage(t, err, "case id")
	})
	t.Run("target before version", func(t *testing.T) {
		req := validReq
		req.To = "bogus"
		req.ExpectedVersion = -1
		_, err := decideRequest(t, baseSnapshot(StatusOpen, 3), req)
		requireIs(t, err, ErrInvalidRequest)
		requireMessage(t, err, "target status")
	})
	t.Run("version before actor", func(t *testing.T) {
		req := validReq
		req.ExpectedVersion = -1
		req.Actor = "broken"
		_, err := decideRequest(t, baseSnapshot(StatusOpen, 3), req)
		requireIs(t, err, ErrInvalidRequest)
		requireMessage(t, err, "expected version")
	})
	t.Run("actor before forbidden resolution", func(t *testing.T) {
		req := request(StatusInAgent, "broken", "", "manual")
		_, err := decideRequest(t, baseSnapshot(StatusOpen, 3), req)
		requireIs(t, err, ErrInvalidRequest)
		requireMessage(t, err, "actor")
	})
	t.Run("forbidden resolution before row lookup", func(t *testing.T) {
		req := request(StatusInAgent, "system:worker", "", "manual")
		_, err := decideRequest(t, baseSnapshot(StatusOpen, 3), req)
		requireIs(t, err, ErrInvalidRequest)
		requireMessage(t, err, "resolution type is forbidden")
	})
	t.Run("stored status before self", func(t *testing.T) {
		_, err := decideRequest(t, baseSnapshot("bogus", 3),
			request(StatusOpen, "human:alice", "reopen", ""))
		requireIs(t, err, ErrInvalidStatus)
	})
	t.Run("self before stale", func(t *testing.T) {
		req := request(StatusOpen, "human:alice", "", "")
		req.ExpectedVersion = 99
		_, err := decideRequest(t, baseSnapshot(StatusOpen, 3), req)
		requireIs(t, err, ErrAlreadyInState)
	})
	t.Run("stale before terminal", func(t *testing.T) {
		req := request(StatusInAgent, "system:worker", "", "")
		req.ExpectedVersion = 99
		_, err := decideRequest(t, baseSnapshot(StatusResolved, 3), req)
		requireIs(t, err, ErrStale)
	})
	t.Run("terminal before edge", func(t *testing.T) {
		_, err := decideRequest(t, baseSnapshot(StatusResolved, 3),
			request(StatusInAgent, "system:worker", "", ""))
		requireIs(t, err, ErrTerminalState)
		requireIs(t, err, ErrIllegalTransition)
	})
	t.Run("edge before actor", func(t *testing.T) {
		_, err := decideRequest(t, baseSnapshot(StatusOpen, 3),
			request(StatusWaitingInfo, "agent:model", "", ""))
		requireIs(t, err, ErrIllegalTransition)
	})
	t.Run("actor before reason", func(t *testing.T) {
		_, err := decideRequest(t, baseSnapshot(StatusOpen, 3),
			request(StatusInAgent, "human:alice", "", ""))
		requireIs(t, err, ErrActorNotAllowed)
	})
	t.Run("agent actor before reason", func(t *testing.T) {
		_, err := decideRequest(t, baseSnapshot(StatusOpen, 3),
			request(StatusEscalated, "agent:model", "", ""))
		requireIs(t, err, ErrActorNotAllowed)
	})
	t.Run("reason before resolution", func(t *testing.T) {
		_, err := decideRequest(t, baseSnapshot(StatusOpen, 3),
			request(StatusResolved, "human:alice", "", ""))
		requireIs(t, err, ErrReasonRequired)
	})
	t.Run("invalid reason before missing resolution", func(t *testing.T) {
		_, err := decideRequest(t, baseSnapshot(StatusOpen, 3),
			request(StatusResolved, "human:alice", strings.Repeat("a", 501), ""))
		requireIs(t, err, ErrInvalidReason)
	})
	t.Run("resolution last", func(t *testing.T) {
		_, err := decideRequest(t, baseSnapshot(StatusOpen, 3),
			request(StatusResolved, "human:alice", "manual", "Bad!"))
		requireIs(t, err, ErrInvalidRequest)
	})
}

// TestTransitionRequestShape covers nil-tx and shape validation through the
// public entry point.
func TestTransitionRequestShape(t *testing.T) {
	if _, err := Transition(t.Context(), nil, request(StatusInAgent, "system:worker", "", "")); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("nil tx error = %v, want ErrInvalidRequest", err)
	}
	tests := []struct {
		name string
		req  TransitionRequest
		want error
	}{
		{"empty case id", TransitionRequest{To: StatusInAgent, ExpectedVersion: 1, Actor: "system:worker"}, ErrInvalidRequest},
		{"malformed case id", TransitionRequest{CaseID: "case-1", To: StatusInAgent, ExpectedVersion: 1, Actor: "system:worker"}, ErrInvalidRequest},
		{"empty target", TransitionRequest{CaseID: testCaseID, ExpectedVersion: 1, Actor: "system:worker"}, ErrInvalidRequest},
		{"unknown target", TransitionRequest{CaseID: testCaseID, To: "agent_failed", ExpectedVersion: 1, Actor: "system:worker"}, ErrInvalidRequest},
		{"negative version", TransitionRequest{CaseID: testCaseID, To: StatusInAgent, ExpectedVersion: -1, Actor: "system:worker"}, ErrInvalidRequest},
		{"malformed actor", TransitionRequest{CaseID: testCaseID, To: StatusInAgent, ExpectedVersion: 1, Actor: "worker"}, ErrInvalidRequest},
		{"forbidden resolution", TransitionRequest{CaseID: testCaseID, To: StatusInAgent, ExpectedVersion: 1, Actor: "system:worker", ResolutionType: "manual"}, ErrInvalidRequest},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseRequest(tc.req)
			requireIs(t, err, tc.want)
		})
	}
	// CreateCase inserts version 0, so a freshly created case must be able to
	// take its first transition without a synthetic version bump.
	if _, err := parseRequest(TransitionRequest{
		CaseID: testCaseID, To: StatusInAgent, ExpectedVersion: 0, Actor: "system:worker",
	}); err != nil {
		t.Fatalf("zero expected version = %v, want accepted for a freshly created case", err)
	}
	parsed, err := parseRequest(request(StatusResolved, "human:alice", "  manual  ", "  manual  "))
	if err != nil {
		t.Fatalf("parseRequest: %v", err)
	}
	if parsed.reason != "manual" || parsed.resolution != "manual" {
		t.Fatalf("parsed reason/resolution = %q/%q, want trimmed manual/manual",
			parsed.reason, parsed.resolution)
	}
}
