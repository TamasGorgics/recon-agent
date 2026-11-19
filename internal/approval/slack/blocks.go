package slack

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/TamasGorgics/recon-agent/internal/money"
)

// Block Kit action identifiers for the three approval buttons. They are part
// of the interaction contract frozen in spec section 5.2 and are exported so
// the interaction handler (wave 4) and tests share one spelling.
const (
	ActionApprove     = "approval_approve"
	ActionReject      = "approval_reject"
	ActionRequestInfo = "approval_request_info"
)

// Builder limits. Every limit is counted in Unicode runes after escaping, so
// a hostile string cannot expand past Slack's section cap.
const (
	maxBlocks         = 50
	maxSectionRunes   = 3000
	maxEvidence       = 5
	maxEvidenceRunes  = 200
	maxRationaleRunes = 1200
	maxHeaderRunes    = 150
	maxButtonRunes    = 75
)

// ErrMessageTooLarge reports a rendered message that would exceed a Slack
// structural limit (block count, header, or section text). It wraps the
// package's ErrInvalidMessage so callers can treat every builder failure as
// one class.
var ErrMessageTooLarge = fmt.Errorf("%w: message exceeds Slack limits", ErrInvalidMessage)

// MessageData is the source-independent content for one approval request. The
// wave-3 Source hydrates it outside any transaction; the builder never reads
// the database. All strings are untrusted and are escaped/capped here.
//
// CaseFileURL is the final browser URL. When empty, BuildOptions.CaseFileBaseURL
// plus "/v1/cases/<case-id>" is used, so production normally sets only the base.
type MessageData struct {
	ProposalID      string
	ProposalType    string
	ProposalVersion int
	ProposalStatus  string
	RiskTier        string
	ExpiresAt       time.Time

	CaseID         string
	CaseValueMinor int64
	CaseCurrency   string
	CasePriority   int
	CaseCreatedAt  time.Time

	BreakCount             int
	PrimaryBreakSide       string
	PrimaryBreakDeltaMinor int64
	PrimaryBreakCurrency   string

	PatternClaimed  string
	PatternVerified string
	// Verification is the stored verifier Details JSON. It carries expected
	// and actual minor units and the verifier currency when available.
	Verification json.RawMessage
	PolicyRule   string
	PolicyTier   string

	Rationale string
	Evidence  []string

	CaseFileURL string
}

// BuildOptions carries server-owned build inputs. ChannelID is copied into the
// Slack message for a stable JSON envelope; Now makes the case-age text
// deterministic in tests and rehydration.
type BuildOptions struct {
	ChannelID       string
	CaseFileBaseURL string
	Now             time.Time
}

// OutgoingMessage is the stable Slack message envelope. JSON field order is channel,
// text, blocks so recorded fixtures are byte-stable. ClientMsgID is transport
// metadata (client_msg_id) and is deliberately not part of this envelope.
type OutgoingMessage struct {
	Channel     string  `json:"channel,omitempty"`
	Text        string  `json:"text"`
	Blocks      []Block `json:"blocks"`
	ClientMsgID string  `json:"-"`
}

// Block is the subset of Block Kit blocks the approval message uses. Unknown
// fields are never populated, so the JSON remains stable.
type Block struct {
	Type     string        `json:"type"`
	Text     *TextObject   `json:"text,omitempty"`
	Elements []BlockAction `json:"elements,omitempty"`
}

// TextObject is a Slack plain_text or mrkdwn text object.
type TextObject struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// BlockAction is one interactive element. Only buttons are emitted today.
type BlockAction struct {
	Type     string      `json:"type"`
	Text     *TextObject `json:"text,omitempty"`
	ActionID string      `json:"action_id,omitempty"`
	Value    string      `json:"value,omitempty"`
	Style    string      `json:"style,omitempty"`
}

// verificationDetails is the subset of proposals.verification the message
// reads. Pointers distinguish "not present" from a legitimate zero amount.
type verificationDetails struct {
	Reason        string `json:"reason"`
	ExpectedMinor *int64 `json:"expected_minor"`
	ActualMinor   *int64 `json:"actual_minor"`
	Currency      string `json:"currency"`
}

// BuildMessage renders MessageData into the frozen approval message. It is
// pure: no clock, database, network, logger, or secret is consulted. The
// message always has one plain-text fallback and the three approval buttons
// with value "<proposalID>:<version>".
func BuildMessage(data MessageData, opts BuildOptions) (OutgoingMessage, error) {
	if err := validateMessageData(data); err != nil {
		return OutgoingMessage{}, err
	}
	details, err := decodeVerification(data.Verification)
	if err != nil {
		return OutgoingMessage{}, err
	}
	if opts.ChannelID != "" {
		if strings.TrimSpace(opts.ChannelID) != opts.ChannelID {
			return OutgoingMessage{}, fmt.Errorf("%w: channel id has surrounding whitespace", ErrInvalidMessage)
		}
	}
	caseURL, err := resolveCaseFileURL(data, opts)
	if err != nil {
		return OutgoingMessage{}, err
	}

	caseShort := shortID(data.CaseID)
	proposalShort := shortID(data.ProposalID)
	tier := data.RiskTier
	if tier == "" {
		tier = data.PolicyTier
	}
	value, err := formatAmount(data.CaseValueMinor, data.CaseCurrency)
	if err != nil {
		return OutgoingMessage{}, err
	}

	fallback := fmt.Sprintf("Approval required: case %s — %s %s %s (%s)",
		caseShort, data.ProposalType, value, data.CaseCurrency, tier)

	blocks := []Block{
		{
			Type: "header",
			Text: &TextObject{
				Type: "plain_text",
				Text: "Approval required · " + caseShort,
			},
		},
		{
			Type: "section",
			Text: &TextObject{
				Type: "mrkdwn",
				Text: caseSummaryText(data, caseShort, value, opts.Now),
			},
		},
		{
			Type: "section",
			Text: &TextObject{
				Type: "mrkdwn",
				Text: deltaText(data, details),
			},
		},
		{
			Type: "section",
			Text: &TextObject{
				Type: "mrkdwn",
				Text: evidenceText(data.Evidence),
			},
		},
		{
			Type: "section",
			Text: &TextObject{
				Type: "mrkdwn",
				Text: verifierText(data, details),
			},
		},
		{
			Type: "section",
			Text: &TextObject{
				Type: "mrkdwn",
				Text: rationaleText(data.Rationale),
			},
		},
		{
			Type: "section",
			Text: &TextObject{
				Type: "mrkdwn",
				Text: metadataText(data, proposalShort, tier),
			},
		},
		{
			Type: "section",
			Text: &TextObject{
				Type: "mrkdwn",
				Text: fmt.Sprintf("<%s|View case file>", caseURL),
			},
		},
		actionsBlock(data),
	}
	if err := validateRendered(blocks); err != nil {
		return OutgoingMessage{}, err
	}

	return OutgoingMessage{
		Channel:     opts.ChannelID,
		Text:        fallback,
		Blocks:      blocks,
		ClientMsgID: data.ProposalID,
	}, nil
}

func validateMessageData(data MessageData) error {
	if !canonicalUUIDPattern.MatchString(data.ProposalID) {
		return fmt.Errorf("%w: proposal id %q is not a canonical lowercase UUID",
			ErrInvalidMessage, data.ProposalID)
	}
	if !canonicalUUIDPattern.MatchString(data.CaseID) {
		return fmt.Errorf("%w: case id %q is not a canonical lowercase UUID",
			ErrInvalidMessage, data.CaseID)
	}
	if data.ProposalVersion <= 0 {
		return fmt.Errorf("%w: proposal version must be positive, got %d",
			ErrInvalidMessage, data.ProposalVersion)
	}
	switch data.ProposalType {
	case "link", "adjustment", "write_off", "duplicate", "request_info":
	default:
		return fmt.Errorf("%w: unknown proposal type %q", ErrInvalidMessage, data.ProposalType)
	}
	tier := data.RiskTier
	if tier == "" {
		tier = data.PolicyTier
	}
	switch tier {
	case "T0", "T1", "T2":
	default:
		return fmt.Errorf("%w: risk tier must be T0, T1, or T2, got %q",
			ErrInvalidMessage, tier)
	}
	if data.PolicyTier != "" {
		switch data.PolicyTier {
		case "T0", "T1", "T2":
		default:
			return fmt.Errorf("%w: policy tier must be T0, T1, or T2, got %q",
				ErrInvalidMessage, data.PolicyTier)
		}
	}
	if _, err := money.ParseCurrency(data.CaseCurrency); err != nil {
		return fmt.Errorf("%w: case currency %q: %v", ErrInvalidMessage, data.CaseCurrency, err)
	}
	if data.CaseValueMinor < 0 {
		return fmt.Errorf("%w: case value must be non-negative, got %d",
			ErrInvalidMessage, data.CaseValueMinor)
	}
	if data.BreakCount < 0 {
		return fmt.Errorf("%w: break count must be non-negative, got %d",
			ErrInvalidMessage, data.BreakCount)
	}
	if data.PrimaryBreakCurrency != "" {
		if _, err := money.ParseCurrency(data.PrimaryBreakCurrency); err != nil {
			return fmt.Errorf("%w: break currency %q: %v",
				ErrInvalidMessage, data.PrimaryBreakCurrency, err)
		}
	}
	return nil
}

func decodeVerification(raw json.RawMessage) (verificationDetails, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return verificationDetails{}, nil
	}
	var details verificationDetails
	if err := json.Unmarshal(raw, &details); err != nil {
		return verificationDetails{}, fmt.Errorf("%w: verifier details are not valid JSON: %v",
			ErrInvalidMessage, err)
	}
	return details, nil
}

func caseSummaryText(data MessageData, caseShort, value string, now time.Time) string {
	first := fmt.Sprintf("*Case* %s · *Type* %s · *Value* %s %s · *Priority* %d",
		caseShort, sanitize(data.ProposalType), value, data.CaseCurrency, data.CasePriority)

	currency := data.PrimaryBreakCurrency
	if currency == "" {
		currency = data.CaseCurrency
	}
	delta, err := formatAmount(data.PrimaryBreakDeltaMinor, currency)
	if err != nil {
		delta = "0"
	}
	var second string
	if data.BreakCount > 1 {
		second = fmt.Sprintf("*Breaks* %d · *Primary* %s · *Delta* %s %s",
			data.BreakCount, sanitizeSide(data.PrimaryBreakSide), delta, currency)
	} else {
		side := sanitizeSide(data.PrimaryBreakSide)
		if data.BreakCount == 0 {
			side = "none"
		}
		second = fmt.Sprintf("*Break* %s · *Delta* %s %s", side, delta, currency)
	}
	second += " · *Age* " + caseAgeAt(data.CaseCreatedAt, now)
	return first + "\n" + second
}

func deltaText(data MessageData, details verificationDetails) string {
	currency := data.PrimaryBreakCurrency
	if currency == "" {
		currency = data.CaseCurrency
	}
	delta, err := formatAmount(data.PrimaryBreakDeltaMinor, currency)
	if err != nil {
		delta = "0"
	}
	text := fmt.Sprintf("*Delta (break)* %s %s", delta, currency)
	if details.ExpectedMinor == nil && details.ActualMinor == nil {
		return text
	}
	verifierCurrency := details.Currency
	if verifierCurrency == "" {
		verifierCurrency = currency
	} else if _, err := money.ParseCurrency(verifierCurrency); err != nil {
		verifierCurrency = currency
	}
	expected := "n/a"
	if details.ExpectedMinor != nil {
		if formatted, err := formatAmount(*details.ExpectedMinor, verifierCurrency); err == nil {
			expected = formatted
		}
	}
	actual := "n/a"
	if details.ActualMinor != nil {
		if formatted, err := formatAmount(*details.ActualMinor, verifierCurrency); err == nil {
			actual = formatted
		}
	}
	return text + "\n" + fmt.Sprintf(
		"*Expected (verifier)* %s %s · *Actual (verifier)* %s %s · *Verifier currency* %s",
		expected, verifierCurrency, actual, verifierCurrency, verifierCurrency)
}

func evidenceText(refs []string) string {
	clean := make([]string, 0, len(refs))
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		clean = append(clean, ref)
	}
	var b strings.Builder
	b.WriteString("*Evidence*")
	if len(clean) == 0 {
		b.WriteString("\nNo evidence references.")
		return b.String()
	}
	shown := clean
	if len(shown) > maxEvidence {
		shown = shown[:maxEvidence]
	}
	for _, ref := range shown {
		b.WriteString("\n• ")
		b.WriteString(capEscaped(sanitize(ref), maxEvidenceRunes))
	}
	if remaining := len(clean) - len(shown); remaining > 0 {
		fmt.Fprintf(&b, "\n+%d more", remaining)
	}
	return b.String()
}

func verifierText(data MessageData, details verificationDetails) string {
	var lines []string
	switch {
	case data.PatternVerified != "":
		lines = append(lines, fmt.Sprintf("*Pattern* %s (verified) · *Result* pass",
			capEscaped(sanitize(data.PatternVerified), 64)))
	case data.PatternClaimed != "":
		lines = append(lines, fmt.Sprintf("*Pattern* %s (claimed, unverified) · *Result* not verified",
			capEscaped(sanitize(data.PatternClaimed), 64)))
	default:
		lines = append(lines, "*Pattern* none · *Result* not verified")
	}
	if details.Reason != "" {
		lines = append(lines, "*Verifier reason* "+
			capEscaped(sanitize(details.Reason), 200))
	}
	return strings.Join(lines, "\n")
}

func rationaleText(rationale string) string {
	body := capEscaped(sanitize(rationale), maxRationaleRunes)
	if strings.TrimSpace(body) == "" {
		body = "No rationale provided."
	}
	return "*Agent rationale*\n" + body
}

func metadataText(data MessageData, proposalShort, tier string) string {
	expires := "none"
	if !data.ExpiresAt.IsZero() {
		expires = data.ExpiresAt.UTC().Format(time.RFC3339)
	}
	text := fmt.Sprintf("*Risk tier* %s · *Proposal version* %d · *Expires* %s · *Proposal* %s",
		sanitize(tier), data.ProposalVersion, sanitize(expires), proposalShort)
	if data.PolicyRule != "" {
		text = fmt.Sprintf("*Risk tier* %s · *Policy rule* %s · *Proposal version* %d · *Expires* %s · *Proposal* %s",
			sanitize(tier), capEscaped(sanitize(data.PolicyRule), 200),
			data.ProposalVersion, sanitize(expires), proposalShort)
	}
	return text
}

func actionsBlock(data MessageData) Block {
	value := data.ProposalID + ":" + strconv.Itoa(data.ProposalVersion)
	return Block{
		Type: "actions",
		Elements: []BlockAction{
			{
				Type:     "button",
				Text:     &TextObject{Type: "plain_text", Text: "Approve"},
				ActionID: ActionApprove,
				Value:    value,
				Style:    "primary",
			},
			{
				Type:     "button",
				Text:     &TextObject{Type: "plain_text", Text: "Reject"},
				ActionID: ActionReject,
				Value:    value,
				Style:    "danger",
			},
			{
				Type:     "button",
				Text:     &TextObject{Type: "plain_text", Text: "Request info"},
				ActionID: ActionRequestInfo,
				Value:    value,
			},
		},
	}
}

func validateRendered(blocks []Block) error {
	if len(blocks) > maxBlocks {
		return fmt.Errorf("%w: %d blocks, maximum is %d",
			ErrMessageTooLarge, len(blocks), maxBlocks)
	}
	for i, block := range blocks {
		if block.Text == nil {
			continue
		}
		limit := maxSectionRunes
		if block.Type == "header" {
			limit = maxHeaderRunes
		}
		if count := utf8.RuneCountInString(block.Text.Text); count > limit {
			return fmt.Errorf("%w: block %d text is %d runes, maximum is %d",
				ErrMessageTooLarge, i, count, limit)
		}
		for j, element := range block.Elements {
			if element.Text == nil {
				continue
			}
			if count := utf8.RuneCountInString(element.Text.Text); count > maxButtonRunes {
				return fmt.Errorf("%w: block %d element %d text is %d runes, maximum is %d",
					ErrMessageTooLarge, i, j, count, maxButtonRunes)
			}
		}
	}
	return nil
}

func resolveCaseFileURL(data MessageData, opts BuildOptions) (string, error) {
	if data.CaseFileURL != "" {
		return validateCaseFileURL(data.CaseFileURL)
	}
	if strings.TrimSpace(opts.CaseFileBaseURL) == "" {
		return "", fmt.Errorf("%w: case file URL is required", ErrInvalidMessage)
	}
	base, err := validateCaseFileBaseURL(opts.CaseFileBaseURL)
	if err != nil {
		return "", err
	}
	return base + "/v1/cases/" + data.CaseID, nil
}

func validateCaseFileBaseURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed != raw || containsUnsafeURLRune(raw) {
		return "", fmt.Errorf("%w: case file base URL is malformed", ErrInvalidMessage)
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Opaque != "" ||
		(!strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https")) ||
		parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", fmt.Errorf("%w: case file base URL must be an absolute http or https URL without userinfo, query, or fragment",
			ErrInvalidMessage)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	return parsed.String(), nil
}

func validateCaseFileURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed != raw || containsUnsafeURLRune(raw) {
		return "", fmt.Errorf("%w: case file URL is malformed", ErrInvalidMessage)
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Opaque != "" ||
		(!strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https")) ||
		parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", fmt.Errorf("%w: case file URL must be an absolute http or https URL without userinfo, query, or fragment",
			ErrInvalidMessage)
	}
	return parsed.String(), nil
}

func containsUnsafeURLRune(raw string) bool {
	for _, r := range raw {
		if r <= 0x20 || r == 0x7f || r == '<' || r == '>' || r == '"' || r == '\'' {
			return true
		}
	}
	return false
}

// sanitize strips ASCII control characters, then escapes the three characters
// Slack mrkdwn treats specially.
func sanitize(s string) string {
	mapped := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	mapped = strings.ReplaceAll(mapped, "&", "&amp;")
	mapped = strings.ReplaceAll(mapped, "<", "&lt;")
	return strings.ReplaceAll(mapped, ">", "&gt;")
}

// capEscaped truncates already-escaped text to max runes including the
// ellipsis, and never leaves a half-written &amp;/&lt;/&gt; entity behind.
func capEscaped(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	cut := max - 1
	for i := cut - 1; i >= 0 && i >= cut-9; i-- {
		if runes[i] != '&' {
			continue
		}
		end := i + 1
		for end < len(runes) && end < i+10 && runes[end] != ';' {
			end++
		}
		if end < len(runes) && runes[end] == ';' && end >= cut {
			cut = i
		}
		break
	}
	if cut <= 0 {
		return "…"
	}
	return string(runes[:cut]) + "…"
}

func sanitizeSide(side string) string {
	side = sanitize(side)
	if strings.TrimSpace(side) == "" {
		return "unknown"
	}
	return capEscaped(side, 40)
}

func caseAgeAt(created, now time.Time) string {
	if created.IsZero() || now.IsZero() {
		return "unknown"
	}
	age := now.Sub(created)
	if age < 0 {
		age = 0
	}
	switch {
	case age < time.Minute:
		return "just now"
	case age < time.Hour:
		return fmt.Sprintf("%dm", int(age/time.Minute))
	case age < 24*time.Hour:
		hours := int(age / time.Hour)
		minutes := int((age % time.Hour) / time.Minute)
		if minutes == 0 {
			return fmt.Sprintf("%dh", hours)
		}
		return fmt.Sprintf("%dh %dm", hours, minutes)
	default:
		days := int(age / (24 * time.Hour))
		hours := int((age % (24 * time.Hour)) / time.Hour)
		if hours == 0 {
			return fmt.Sprintf("%dd", days)
		}
		return fmt.Sprintf("%dd %dh", days, hours)
	}
}

func shortID(id string) string {
	compact := strings.ReplaceAll(id, "-", "")
	if len(compact) < 8 {
		return compact
	}
	return compact[:8]
}

func formatAmount(minor int64, currency string) (string, error) {
	parsed, err := money.ParseCurrency(currency)
	if err != nil {
		return "", fmt.Errorf("%w: currency %q: %v", ErrInvalidMessage, currency, err)
	}
	return money.FromMinor(minor, parsed).DecimalString(), nil
}
