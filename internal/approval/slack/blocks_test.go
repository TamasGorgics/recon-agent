package slack_test

import (
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/TamasGorgics/recon-agent/internal/approval/slack"
)

var updateGolden = flag.Bool("update", false, "rewrite slack testdata goldens")

type buildFixture struct {
	Now             string         `json:"now"`
	ChannelID       string         `json:"channel_id"`
	CaseFileBaseURL string         `json:"case_file_base_url"`
	Data            messageFixture `json:"data"`
}

type messageFixture struct {
	ProposalID             string          `json:"proposal_id"`
	ProposalType           string          `json:"proposal_type"`
	ProposalVersion        int             `json:"proposal_version"`
	ProposalStatus         string          `json:"proposal_status"`
	RiskTier               string          `json:"risk_tier"`
	ExpiresAt              string          `json:"expires_at"`
	CaseID                 string          `json:"case_id"`
	CaseValueMinor         int64           `json:"case_value_minor"`
	CaseCurrency           string          `json:"case_currency"`
	CasePriority           int             `json:"case_priority"`
	CaseCreatedAt          string          `json:"case_created_at"`
	BreakCount             int             `json:"break_count"`
	PrimaryBreakSide       string          `json:"primary_break_side"`
	PrimaryBreakDeltaMinor int64           `json:"primary_break_delta_minor"`
	PrimaryBreakCurrency   string          `json:"primary_break_currency"`
	PatternClaimed         string          `json:"pattern_claimed"`
	PatternVerified        string          `json:"pattern_verified"`
	Verification           json.RawMessage `json:"verification"`
	PolicyRule             string          `json:"policy_rule"`
	PolicyTier             string          `json:"policy_tier"`
	Rationale              string          `json:"rationale"`
	Evidence               []string        `json:"evidence"`
	CaseFileURL            string          `json:"case_file_url"`
}

func readBuildFixture(t *testing.T, name string) buildFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "blocks", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	var fixture buildFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("decode fixture %s: %v", name, err)
	}
	return fixture
}

func (f buildFixture) options(t *testing.T) slack.BuildOptions {
	t.Helper()
	now, err := time.Parse(time.RFC3339Nano, f.Now)
	if err != nil {
		t.Fatalf("parse fixture now: %v", err)
	}
	return slack.BuildOptions{
		ChannelID:       f.ChannelID,
		CaseFileBaseURL: f.CaseFileBaseURL,
		Now:             now,
	}
}

func (f buildFixture) messageData(t *testing.T) slack.MessageData {
	t.Helper()
	var (
		expires time.Time
		created time.Time
		err     error
	)
	if f.Data.ExpiresAt != "" {
		expires, err = time.Parse(time.RFC3339Nano, f.Data.ExpiresAt)
		if err != nil {
			t.Fatalf("parse fixture expires_at: %v", err)
		}
	}
	if f.Data.CaseCreatedAt != "" {
		created, err = time.Parse(time.RFC3339Nano, f.Data.CaseCreatedAt)
		if err != nil {
			t.Fatalf("parse fixture case_created_at: %v", err)
		}
	}
	return slack.MessageData{
		ProposalID:             f.Data.ProposalID,
		ProposalType:           f.Data.ProposalType,
		ProposalVersion:        f.Data.ProposalVersion,
		ProposalStatus:         f.Data.ProposalStatus,
		RiskTier:               f.Data.RiskTier,
		ExpiresAt:              expires,
		CaseID:                 f.Data.CaseID,
		CaseValueMinor:         f.Data.CaseValueMinor,
		CaseCurrency:           f.Data.CaseCurrency,
		CasePriority:           f.Data.CasePriority,
		CaseCreatedAt:          created,
		BreakCount:             f.Data.BreakCount,
		PrimaryBreakSide:       f.Data.PrimaryBreakSide,
		PrimaryBreakDeltaMinor: f.Data.PrimaryBreakDeltaMinor,
		PrimaryBreakCurrency:   f.Data.PrimaryBreakCurrency,
		PatternClaimed:         f.Data.PatternClaimed,
		PatternVerified:        f.Data.PatternVerified,
		Verification:           f.Data.Verification,
		PolicyRule:             f.Data.PolicyRule,
		PolicyTier:             f.Data.PolicyTier,
		Rationale:              f.Data.Rationale,
		Evidence:               append([]string(nil), f.Data.Evidence...),
		CaseFileURL:            f.Data.CaseFileURL,
	}
}

func buildFixtureMessage(t *testing.T, fixture buildFixture) slack.OutgoingMessage {
	t.Helper()
	message, err := slack.BuildMessage(fixture.messageData(t), fixture.options(t))
	if err != nil {
		t.Fatalf("BuildMessage: %v", err)
	}
	return message
}

func assertGolden(t *testing.T, name string, value any) {
	t.Helper()
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("marshal golden: %v", err)
	}
	encoded = append(encoded, '\n')
	path := filepath.Join("testdata", "blocks", name)
	if *updateGolden {
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			t.Fatalf("write golden %s: %v", name, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run go test -run BuildMessage -update): %v", name, err)
	}
	if string(encoded) != string(want) {
		t.Fatalf("golden %s mismatch\n--- got ---\n%s\n--- want ---\n%s", name, encoded, want)
	}
}

func TestBuildMessageGolden(t *testing.T) {
	fixture := readBuildFixture(t, "t1_input.json")
	message := buildFixtureMessage(t, fixture)
	assertGolden(t, "t1_golden.json", message)
}

func TestBuildMessageHostileGolden(t *testing.T) {
	fixture := readBuildFixture(t, "hostile_input.json")
	message := buildFixtureMessage(t, fixture)
	assertGolden(t, "hostile_golden.json", message)
}

func TestBuildMessageHeaderFallbackAndActions(t *testing.T) {
	fixture := readBuildFixture(t, "t1_input.json")
	message := buildFixtureMessage(t, fixture)

	if message.Text != "Approval required: case aaaaaaaa — write_off 1234.56 USD (T1)" {
		t.Fatalf("fallback text = %q", message.Text)
	}
	if message.Channel != "C0123456789" {
		t.Fatalf("message channel = %q", message.Channel)
	}
	if message.ClientMsgID != fixture.Data.ProposalID {
		t.Fatalf("client msg id = %q, want proposal id", message.ClientMsgID)
	}
	if len(message.Blocks) != 9 {
		t.Fatalf("block count = %d, want 9", len(message.Blocks))
	}
	header := message.Blocks[0]
	if header.Type != "header" || header.Text == nil || header.Text.Type != "plain_text" {
		t.Fatalf("header block = %#v", header)
	}
	if header.Text.Text != "Approval required · aaaaaaaa" {
		t.Fatalf("header text = %q", header.Text.Text)
	}

	actions := message.Blocks[len(message.Blocks)-1]
	if actions.Type != "actions" || len(actions.Elements) != 3 {
		t.Fatalf("actions block = %#v", actions)
	}
	wantActions := []struct {
		action string
		text   string
		style  string
	}{
		{slack.ActionApprove, "Approve", "primary"},
		{slack.ActionReject, "Reject", "danger"},
		{slack.ActionRequestInfo, "Request info", ""},
	}
	wantValue := fixture.Data.ProposalID + ":3"
	for i, want := range wantActions {
		element := actions.Elements[i]
		if element.Type != "button" || element.ActionID != want.action {
			t.Fatalf("button %d = %#v, want action %q", i, element, want.action)
		}
		if element.Text == nil || element.Text.Text != want.text {
			t.Fatalf("button %d text = %#v, want %q", i, element.Text, want.text)
		}
		if element.Style != want.style {
			t.Fatalf("button %d style = %q, want %q", i, element.Style, want.style)
		}
		if element.Value != wantValue {
			t.Fatalf("button %d value = %q, want %q", i, element.Value, wantValue)
		}
	}
}

func TestBuildMessageStableJSONEnvelope(t *testing.T) {
	fixture := readBuildFixture(t, "t1_input.json")
	message := buildFixtureMessage(t, fixture)
	first, err := json.Marshal(message)
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}
	second, err := json.Marshal(message)
	if err != nil {
		t.Fatalf("marshal message again: %v", err)
	}
	if string(first) != string(second) {
		t.Fatal("message JSON is not stable across marshals")
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(first, &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if len(envelope) != 3 {
		t.Fatalf("envelope keys = %v, want exactly channel, text, blocks", envelope)
	}
	for _, key := range []string{"channel", "text", "blocks"} {
		if _, ok := envelope[key]; !ok {
			t.Fatalf("envelope is missing %q", key)
		}
	}
	if _, ok := envelope["client_msg_id"]; ok {
		t.Fatal("client_msg_id must not be part of the stable message envelope")
	}
}

func TestBuildMessageEscapesAndStripsControls(t *testing.T) {
	fixture := readBuildFixture(t, "hostile_input.json")
	message := buildFixtureMessage(t, fixture)
	encoded, err := json.Marshal(message)
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}
	var decoded slack.OutgoingMessage
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode message: %v", err)
	}
	var content strings.Builder
	content.WriteString(decoded.Text)
	for _, block := range decoded.Blocks {
		if block.Text != nil {
			content.WriteString("\n")
			content.WriteString(block.Text.Text)
		}
		for _, element := range block.Elements {
			if element.Text != nil {
				content.WriteString("\n")
				content.WriteString(element.Text.Text)
			}
		}
	}
	rendered := content.String()
	if strings.Contains(rendered, "<script>") {
		t.Fatal("rendered message contains an unescaped <script> tag")
	}
	if !strings.Contains(rendered, "&lt;script&gt;") {
		t.Fatal("hostile <script> was not escaped")
	}
	if !strings.Contains(rendered, "&amp;") {
		t.Fatal("hostile & was not escaped")
	}
	for _, r := range rendered {
		if r == '\n' {
			continue
		}
		if r < 0x20 || r == 0x7f {
			t.Fatalf("rendered message contains control character %U", r)
		}
	}
}

func TestBuildMessageRationaleAndEvidenceCaps(t *testing.T) {
	fixture := readBuildFixture(t, "t1_input.json")
	data := fixture.messageData(t)
	data.Rationale = strings.Repeat("<", 5000)
	refs := make([]string, 100)
	for i := range refs {
		refs[i] = "ext:bank:" + strings.Repeat("a", 400)
	}
	data.Evidence = refs

	message, err := slack.BuildMessage(data, fixture.options(t))
	if err != nil {
		t.Fatalf("BuildMessage: %v", err)
	}
	rationale := blockText(message, 5)
	body := strings.TrimPrefix(rationale, "*Agent rationale*\n")
	if count := utf8.RuneCountInString(body); count > 1200 {
		t.Fatalf("rationale body is %d runes, want at most 1200", count)
	}
	if !strings.HasSuffix(body, "…") {
		t.Fatalf("truncated rationale does not end with an ellipsis: %q", body)
	}

	evidence := blockText(message, 3)
	if got := strings.Count(evidence, "\n• "); got != 5 {
		t.Fatalf("evidence bullets = %d, want 5\n%s", got, evidence)
	}
	if !strings.Contains(evidence, "+95 more") {
		t.Fatalf("evidence does not report the remaining count:\n%s", evidence)
	}
	for _, line := range strings.Split(evidence, "\n") {
		if !strings.HasPrefix(line, "• ") {
			continue
		}
		if count := utf8.RuneCountInString(strings.TrimPrefix(line, "• ")); count > 200 {
			t.Fatalf("evidence ref is %d runes, want at most 200", count)
		}
	}
}

func TestBuildMessageVerifierStates(t *testing.T) {
	fixture := readBuildFixture(t, "t1_input.json")
	base := fixture.messageData(t)

	t.Run("missing verification is not a pass", func(t *testing.T) {
		data := base
		data.PatternClaimed = ""
		data.PatternVerified = ""
		data.Verification = nil
		message, err := slack.BuildMessage(data, fixture.options(t))
		if err != nil {
			t.Fatalf("BuildMessage: %v", err)
		}
		text := blockText(message, 4)
		if !strings.Contains(text, "not verified") {
			t.Fatalf("verifier text = %q, want not verified", text)
		}
		if strings.Contains(text, "*Result* pass") {
			t.Fatalf("missing verification rendered as a pass: %q", text)
		}
	})

	t.Run("claimed but unverified", func(t *testing.T) {
		data := base
		data.PatternVerified = ""
		data.Verification = json.RawMessage(`{"schema_version":1,"reason":"fee_mismatch"}`)
		message, err := slack.BuildMessage(data, fixture.options(t))
		if err != nil {
			t.Fatalf("BuildMessage: %v", err)
		}
		text := blockText(message, 4)
		if !strings.Contains(text, "(claimed, unverified)") {
			t.Fatalf("verifier text = %q, want claimed, unverified", text)
		}
		if !strings.Contains(text, "fee_mismatch") {
			t.Fatalf("verifier text = %q, want verifier reason", text)
		}
	})
}

func TestBuildMessageMultipleBreaks(t *testing.T) {
	fixture := readBuildFixture(t, "t1_input.json")
	data := fixture.messageData(t)
	data.BreakCount = 4
	data.PrimaryBreakSide = "amount_mismatch"
	message, err := slack.BuildMessage(data, fixture.options(t))
	if err != nil {
		t.Fatalf("BuildMessage: %v", err)
	}
	summary := blockText(message, 1)
	if !strings.Contains(summary, "*Breaks* 4") {
		t.Fatalf("summary = %q, want a 4-break summary", summary)
	}
	if !strings.Contains(summary, "*Primary* amount_mismatch") {
		t.Fatalf("summary = %q, want the primary break", summary)
	}
}

func TestBuildMessageCaseFileLink(t *testing.T) {
	fixture := readBuildFixture(t, "t1_input.json")
	data := fixture.messageData(t)
	options := fixture.options(t)

	message, err := slack.BuildMessage(data, options)
	if err != nil {
		t.Fatalf("BuildMessage: %v", err)
	}
	want := "<https://cases.example.test/v1/cases/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee|View case file>"
	if got := blockText(message, 7); got != want {
		t.Fatalf("case link = %q, want %q", got, want)
	}

	data.CaseFileURL = "https://ui.example.test/cases/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	options.CaseFileBaseURL = ""
	message, err = slack.BuildMessage(data, options)
	if err != nil {
		t.Fatalf("BuildMessage with explicit URL: %v", err)
	}
	if got := blockText(message, 7); !strings.Contains(got, "https://ui.example.test/cases/") {
		t.Fatalf("case link did not use the explicit URL: %q", got)
	}
}

func TestBuildMessageRejectsInvalidInput(t *testing.T) {
	fixture := readBuildFixture(t, "t1_input.json")
	base := fixture.messageData(t)
	options := fixture.options(t)

	tests := []struct {
		name   string
		mutate func(*slack.MessageData, *slack.BuildOptions)
		want   error
	}{
		{
			name: "non-canonical proposal id",
			mutate: func(d *slack.MessageData, _ *slack.BuildOptions) {
				d.ProposalID = "11111111-2222-3333-4444-55555555555Z"
			},
			want: slack.ErrInvalidMessage,
		},
		{
			name: "non-canonical case id",
			mutate: func(d *slack.MessageData, _ *slack.BuildOptions) {
				d.CaseID = "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE"
			},
			want: slack.ErrInvalidMessage,
		},
		{
			name: "zero proposal version",
			mutate: func(d *slack.MessageData, _ *slack.BuildOptions) {
				d.ProposalVersion = 0
			},
			want: slack.ErrInvalidMessage,
		},
		{
			name: "unknown proposal type",
			mutate: func(d *slack.MessageData, _ *slack.BuildOptions) {
				d.ProposalType = "wire_transfer"
			},
			want: slack.ErrInvalidMessage,
		},
		{
			name: "unknown tier",
			mutate: func(d *slack.MessageData, _ *slack.BuildOptions) {
				d.RiskTier = "T3"
			},
			want: slack.ErrInvalidMessage,
		},
		{
			name: "invalid currency",
			mutate: func(d *slack.MessageData, _ *slack.BuildOptions) {
				d.CaseCurrency = "US"
			},
			want: slack.ErrInvalidMessage,
		},
		{
			name: "negative case value",
			mutate: func(d *slack.MessageData, _ *slack.BuildOptions) {
				d.CaseValueMinor = -1
			},
			want: slack.ErrInvalidMessage,
		},
		{
			name: "malformed verifier JSON",
			mutate: func(d *slack.MessageData, _ *slack.BuildOptions) {
				d.Verification = json.RawMessage("{")
			},
			want: slack.ErrInvalidMessage,
		},
		{
			name: "non-http case URL",
			mutate: func(d *slack.MessageData, o *slack.BuildOptions) {
				d.CaseFileURL = "javascript:alert(1)"
				o.CaseFileBaseURL = ""
			},
			want: slack.ErrInvalidMessage,
		},
		{
			name: "case URL with userinfo",
			mutate: func(d *slack.MessageData, o *slack.BuildOptions) {
				d.CaseFileURL = "https://user:pass@cases.example.test/v1/cases/x"
				o.CaseFileBaseURL = ""
			},
			want: slack.ErrInvalidMessage,
		},
		{
			name: "missing case URL",
			mutate: func(d *slack.MessageData, o *slack.BuildOptions) {
				d.CaseFileURL = ""
				o.CaseFileBaseURL = ""
			},
			want: slack.ErrInvalidMessage,
		},
		{
			name: "over-limit case link",
			mutate: func(d *slack.MessageData, o *slack.BuildOptions) {
				d.CaseFileURL = "https://cases.example.test/" + strings.Repeat("a", 4000)
				o.CaseFileBaseURL = ""
			},
			want: slack.ErrMessageTooLarge,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data := base
			opts := options
			test.mutate(&data, &opts)
			_, err := slack.BuildMessage(data, opts)
			if !errors.Is(err, test.want) {
				t.Fatalf("BuildMessage error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestBuildMessageNoSecrets(t *testing.T) {
	fixture := readBuildFixture(t, "t1_input.json")
	message := buildFixtureMessage(t, fixture)
	encoded, err := json.Marshal(message)
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}
	for _, secret := range []string{"xoxb-", "Bearer ", "SLACK_BOT_TOKEN", "SLACK_SIGNING_SECRET"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("message contains secret-shaped text %q", secret)
		}
	}
}

func blockText(message slack.OutgoingMessage, index int) string {
	if index < 0 || index >= len(message.Blocks) {
		return ""
	}
	if message.Blocks[index].Text == nil {
		return ""
	}
	return message.Blocks[index].Text.Text
}
