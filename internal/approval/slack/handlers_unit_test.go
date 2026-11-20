package slack

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/approval"
	caselifecycle "github.com/TamasGorgics/recon-agent/internal/cases/lifecycle"
)

func testMessageData() MessageData {
	return MessageData{
		ProposalID:             "11111111-2222-3333-4444-555555555555",
		ProposalType:           "write_off",
		ProposalVersion:        3,
		ProposalStatus:         "pending_approval",
		RiskTier:               "T1",
		ExpiresAt:              time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC),
		CaseID:                 "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		CaseValueMinor:         123456,
		CaseCurrency:           "USD",
		CasePriority:           2,
		CaseCreatedAt:          time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC),
		BreakCount:             1,
		PrimaryBreakSide:       "external_only",
		PrimaryBreakDeltaMinor: 1250,
		PrimaryBreakCurrency:   "USD",
		PatternVerified:        "fee_variance",
		Verification:           json.RawMessage(`{"reason":"within fee schedule"}`),
		PolicyRule:             "high_value",
		PolicyTier:             "T1",
		Rationale:              "The fee matches the schedule.",
		Evidence:               []string{"ext:bank:abc123"},
	}
}

func testBuildOptions() BuildOptions {
	return BuildOptions{
		ChannelID:       "C0123456789",
		CaseFileBaseURL: "https://cases.example.test",
		Now:             time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC),
	}
}

func hasActions(blocks []Block) bool {
	for _, block := range blocks {
		if block.Type == "actions" {
			return true
		}
	}
	return false
}

func TestParseExternalRef(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		ref     string
		channel string
		ts      string
		ok      bool
	}{
		{"good", "C0123456789:1732041600.123456", "C0123456789", "1732041600.123456", true},
		{"missing colon", "C01234567891732041600.123456", "", "", false},
		{"extra colon", "C0123456789:1:2", "", "", false},
		{"empty channel", ":1732041600.123456", "", "", false},
		{"empty ts", "C0123456789:", "", "", false},
		{"empty", "", "", "", false},
		{"padded channel", " C0123456789:1", "", "", false},
		{"padded ts", "C0123456789: 1", "", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			channel, ts, err := ParseExternalRef(test.ref)
			if test.ok {
				if err != nil {
					t.Fatalf("ParseExternalRef(%q) error = %v", test.ref, err)
				}
				if channel != test.channel || ts != test.ts {
					t.Fatalf("ParseExternalRef(%q) = %q/%q, want %q/%q",
						test.ref, channel, ts, test.channel, test.ts)
				}
				return
			}
			if !errors.Is(err, ErrInvalidMessage) {
				t.Fatalf("ParseExternalRef(%q) error = %v, want ErrInvalidMessage", test.ref, err)
			}
		})
	}
}

func TestParseApprovalRequestPayload(t *testing.T) {
	t.Parallel()
	valid := `{"proposal_id":"11111111-2222-3333-4444-555555555555",` +
		`"case_id":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","tier":"T1",` +
		`"channel":"slack","requested_at":"2026-11-19T12:00:00.123456789Z"}`
	payload, err := parseApprovalRequestPayload(json.RawMessage(valid))
	if err != nil {
		t.Fatalf("parse valid request: %v", err)
	}
	if payload.ProposalID != "11111111-2222-3333-4444-555555555555" ||
		payload.Channel != approval.ChannelSlack || payload.Tier != "T1" {
		t.Fatalf("parsed payload = %+v", payload)
	}

	ticket := `{"proposal_id":"11111111-2222-3333-4444-555555555555",` +
		`"case_id":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","tier":"T2",` +
		`"channel":"ticket","requested_at":"2026-11-19T12:00:00Z"}`
	payload, err = parseApprovalRequestPayload(json.RawMessage(ticket))
	if err != nil {
		t.Fatalf("parse ticket request: %v", err)
	}
	if payload.Channel != approval.ChannelTicket {
		t.Fatalf("ticket channel = %q", payload.Channel)
	}

	for _, test := range []struct {
		name string
		raw  string
	}{
		{"empty", ""},
		{"not json", "{"},
		{"missing proposal", `{"channel":"slack"}`},
		{"bad proposal", `{"proposal_id":"not-a-uuid","channel":"slack"}`},
		{"empty channel", `{"proposal_id":"11111111-2222-3333-4444-555555555555","channel":""}`},
		{"bad case", `{"proposal_id":"11111111-2222-3333-4444-555555555555",` +
			`"case_id":"nope","tier":"T1","channel":"slack"}`},
		{"bad tier", `{"proposal_id":"11111111-2222-3333-4444-555555555555",` +
			`"case_id":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","tier":"T0","channel":"slack"}`},
		{"bad requested_at", `{"proposal_id":"11111111-2222-3333-4444-555555555555",` +
			`"case_id":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","tier":"T1","channel":"slack",` +
			`"requested_at":"yesterday"}`},
		{"unknown field", `{"proposal_id":"11111111-2222-3333-4444-555555555555",` +
			`"channel":"slack","surprise":true}`},
		{"trailing data", `{"proposal_id":"11111111-2222-3333-4444-555555555555",` +
			`"channel":"slack"} {}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseApprovalRequestPayload(json.RawMessage(test.raw)); !errors.Is(err, ErrInvalidPayload) {
				t.Fatalf("parse error = %v, want ErrInvalidPayload", err)
			}
		})
	}
}

func TestParseApprovalUpdatePayload(t *testing.T) {
	t.Parallel()
	valid := `{"proposal_id":"11111111-2222-3333-4444-555555555555",` +
		`"case_id":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","decision":"approved",` +
		`"status":"approved","source":"slack",` +
		`"approver_id":"99999999-8888-7777-6666-555555555555",` +
		`"decided_at":"2026-11-19T12:00:00.123456789Z","info_requested":false}`
	parsed, err := parseApprovalUpdatePayload(json.RawMessage(valid))
	if err != nil {
		t.Fatalf("parse valid update: %v", err)
	}
	if parsed.Decision != approval.DecisionApproved || parsed.Source != "slack" ||
		!parsed.DecidedAt.Equal(time.Date(2026, 11, 19, 12, 0, 0, 123456789, time.UTC)) {
		t.Fatalf("parsed update = %+v", parsed)
	}

	for _, test := range []struct {
		name string
		raw  string
	}{
		{"empty", ""},
		{"missing proposal", `{"case_id":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}`},
		{"bad case", `{"proposal_id":"11111111-2222-3333-4444-555555555555",` +
			`"case_id":"nope","decision":"approved","status":"approved","source":"slack",` +
			`"approver_id":"99999999-8888-7777-6666-555555555555",` +
			`"decided_at":"2026-11-19T12:00:00Z"}`},
		{"bad approver", `{"proposal_id":"11111111-2222-3333-4444-555555555555",` +
			`"case_id":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","decision":"approved",` +
			`"status":"approved","source":"slack","approver_id":"system:bot",` +
			`"decided_at":"2026-11-19T12:00:00Z"}`},
		{"bad decision", `{"proposal_id":"11111111-2222-3333-4444-555555555555",` +
			`"case_id":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","decision":"maybe",` +
			`"status":"approved","source":"slack",` +
			`"approver_id":"99999999-8888-7777-6666-555555555555",` +
			`"decided_at":"2026-11-19T12:00:00Z"}`},
		{"status mismatch", `{"proposal_id":"11111111-2222-3333-4444-555555555555",` +
			`"case_id":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","decision":"approved",` +
			`"status":"rejected","source":"slack",` +
			`"approver_id":"99999999-8888-7777-6666-555555555555",` +
			`"decided_at":"2026-11-19T12:00:00Z"}`},
		{"info requested approved", `{"proposal_id":"11111111-2222-3333-4444-555555555555",` +
			`"case_id":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","decision":"approved",` +
			`"status":"approved","source":"slack",` +
			`"approver_id":"99999999-8888-7777-6666-555555555555",` +
			`"decided_at":"2026-11-19T12:00:00Z","info_requested":true}`},
		{"empty source", `{"proposal_id":"11111111-2222-3333-4444-555555555555",` +
			`"case_id":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","decision":"approved",` +
			`"status":"approved","source":"","approver_id":"99999999-8888-7777-6666-555555555555",` +
			`"decided_at":"2026-11-19T12:00:00Z"}`},
		{"missing decided_at", `{"proposal_id":"11111111-2222-3333-4444-555555555555",` +
			`"case_id":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","decision":"approved",` +
			`"status":"approved","source":"slack",` +
			`"approver_id":"99999999-8888-7777-6666-555555555555"}`},
		{"bad decided_at", `{"proposal_id":"11111111-2222-3333-4444-555555555555",` +
			`"case_id":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","decision":"approved",` +
			`"status":"approved","source":"slack",` +
			`"approver_id":"99999999-8888-7777-6666-555555555555",` +
			`"decided_at":"soon"}`},
		{"unknown field", `{"proposal_id":"11111111-2222-3333-4444-555555555555",` +
			`"case_id":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","decision":"approved",` +
			`"status":"approved","source":"slack",` +
			`"approver_id":"99999999-8888-7777-6666-555555555555",` +
			`"decided_at":"2026-11-19T12:00:00Z","surprise":true}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseApprovalUpdatePayload(json.RawMessage(test.raw)); !errors.Is(err, ErrInvalidPayload) {
				t.Fatalf("parse error = %v, want ErrInvalidPayload", err)
			}
		})
	}
}

func TestParseApprovalCancelPayload(t *testing.T) {
	t.Parallel()
	valid := `{"proposal_id":"11111111-2222-3333-4444-555555555555",` +
		`"case_id":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","reason":"superseded_by_match"}`
	payload, err := parseApprovalCancelPayload(json.RawMessage(valid))
	if err != nil {
		t.Fatalf("parse valid cancel: %v", err)
	}
	if payload.Reason != "superseded_by_match" {
		t.Fatalf("reason = %q", payload.Reason)
	}
	for _, raw := range []string{
		``,
		`{`,
		`{"proposal_id":"11111111-2222-3333-4444-555555555555"}`,
		`{"proposal_id":"11111111-2222-3333-4444-555555555555","case_id":"nope"}`,
		`{"proposal_id":"11111111-2222-3333-4444-555555555555",` +
			`"case_id":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","extra":1}`,
	} {
		if _, err := parseApprovalCancelPayload(json.RawMessage(raw)); !errors.Is(err, ErrInvalidPayload) {
			t.Fatalf("parse %q error = %v, want ErrInvalidPayload", raw, err)
		}
	}
}

func TestHandlerMapKeys(t *testing.T) {
	t.Parallel()
	handlers := (&Handlers{}).Map()
	if len(handlers) != 3 {
		t.Fatalf("handler count = %d, want 3", len(handlers))
	}
	for _, topic := range []string{
		approval.TopicApprovalRequest,
		approval.TopicApprovalUpdate,
		caselifecycle.TopicApprovalCancel,
	} {
		if _, ok := handlers[topic]; !ok {
			t.Fatalf("topic %q is not registered", topic)
		}
	}
}

func TestBuildDecidedMessageHasNoActions(t *testing.T) {
	t.Parallel()
	decidedAt := time.Date(2026, 11, 19, 12, 30, 0, 0, time.UTC)
	message, err := buildDecidedMessage(testMessageData(), testBuildOptions(), decidedNotice{
		Decision:  approval.DecisionApproved,
		Source:    "slack",
		Approver:  "Ada <Lovelace> & Co",
		DecidedAt: decidedAt,
	})
	if err != nil {
		t.Fatalf("buildDecidedMessage: %v", err)
	}
	if hasActions(message.Blocks) {
		t.Fatal("decided message still has an actions block")
	}
	if !strings.Contains(message.Text, "Approval approved") {
		t.Fatalf("fallback = %q", message.Text)
	}
	if message.Blocks[0].Text.Text != "Approval approved · aaaaaaaa" {
		t.Fatalf("header = %q", message.Blocks[0].Text.Text)
	}
	joined := renderBlockText(message.Blocks)
	for _, want := range []string{
		"*Decision* Approved",
		"*Approver* Ada &lt;Lovelace&gt; &amp; Co",
		"*Decided* 2026-11-19T12:30:00Z",
		"*Source* slack",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("decided message missing %q in:\n%s", want, joined)
		}
	}
}

func TestBuildDecidedMessageInfoRequested(t *testing.T) {
	t.Parallel()
	message, err := buildDecidedMessage(testMessageData(), testBuildOptions(), decidedNotice{
		Decision:      approval.DecisionRejected,
		Source:        "slack",
		Approver:      "Ada",
		DecidedAt:     time.Date(2026, 11, 19, 12, 30, 0, 0, time.UTC),
		InfoRequested: true,
		Reason:        "Need the bank statement",
	})
	if err != nil {
		t.Fatalf("buildDecidedMessage: %v", err)
	}
	if hasActions(message.Blocks) {
		t.Fatal("info-requested message still has an actions block")
	}
	if !strings.HasPrefix(message.Text, "Info requested: case") {
		t.Fatalf("fallback = %q", message.Text)
	}
	joined := renderBlockText(message.Blocks)
	if !strings.Contains(joined, "Rejected — information requested") {
		t.Fatalf("missing info-requested label:\n%s", joined)
	}
	if !strings.Contains(joined, "*Reason* Need the bank statement") {
		t.Fatalf("missing reason:\n%s", joined)
	}
}

func TestBuildWithdrawnMessageEscapesAndCapsReason(t *testing.T) {
	t.Parallel()
	reason := "<script>&" + strings.Repeat("x", 900)
	message, err := buildWithdrawnMessage(testMessageData(), testBuildOptions(), reason)
	if err != nil {
		t.Fatalf("buildWithdrawnMessage: %v", err)
	}
	if hasActions(message.Blocks) {
		t.Fatal("withdrawn message still has an actions block")
	}
	if !strings.HasPrefix(message.Text, "Approval withdrawn: case") {
		t.Fatalf("fallback = %q", message.Text)
	}
	joined := renderBlockText(message.Blocks)
	if strings.Contains(joined, "<script>") {
		t.Fatal("withdrawn message rendered raw HTML")
	}
	if !strings.Contains(joined, "&lt;script&gt;&amp;") {
		t.Fatalf("reason was not escaped:\n%s", joined)
	}
	if !strings.Contains(joined, "*Status* Withdrawn") {
		t.Fatalf("missing status:\n%s", joined)
	}
	// Reason is capped at 500 runes by capEscaped; the rendered line must not
	// include all 900 x's.
	if strings.Contains(joined, strings.Repeat("x", 600)) {
		t.Fatal("withdrawn reason was not capped")
	}
}

func renderBlockText(blocks []Block) string {
	var b strings.Builder
	for _, block := range blocks {
		if block.Text != nil {
			b.WriteString(block.Text.Text)
			b.WriteString("\n")
		}
	}
	return b.String()
}
