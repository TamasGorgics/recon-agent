package untrusted

import "regexp"

// defaultPatternSpecs is the built-in detection rule set. Every expression is
// RE2-compatible. The families cover instruction overrides, role/chattemplate
// markers, tool-call lookalikes, URLs, and exfiltration. The exfiltration rules
// are deliberately independent of URL detection: exfiltrate_ledger.txt has no
// URL and must still be caught (spec section 3.5).
var defaultPatternSpecs = []struct {
	name string
	code string
	expr string
}{
	// instruction_override
	{"ignore_previous_instructions", "instruction_override", `(?i)\bignore\s+(?:all\s+)?(?:previous|prior|above)\s+instructions?\b`},
	{"system_override", "instruction_override", `(?i)\bsystem\s+override\b`},
	{"policy_suspended", "instruction_override", `(?i)\bpolicy\s+is\s+suspended\b`},
	{"you_must_set", "instruction_override", `(?i)\byou\s+must\s+set\b`},

	// role_marker
	{"system_role_marker", "role_marker", `(?i)\b(?:system|assistant)\b`},
	{"role_prefix", "role_marker", `(?i)\brole\s*:`},
	{"you_are", "role_marker", `(?i)\byou\s+are\b`},
	{"chat_template_marker", "role_marker", `(?i)<\|\s*(?:im_start|im_end|system|assistant|user)\s*\|>|\[/?INST\]|<<SYS>>|###\s*(?:system|assistant|user)\b`},

	// tool_call_lookalike
	{"json_tool_key", "tool_call_lookalike", `(?i)\"tool\"\s*:`},
	{"propose_tool_name", "tool_call_lookalike", `(?i)\"name\"\s*:\s*\"propose_`},
	{"tool_call_tag", "tool_call_lookalike", `(?i)<\s*/?\s*tool_call\s*>`},
	{"function_call", "tool_call_lookalike", `(?i)\bfunction_call\b`},

	// url
	{"scheme_url", "url", `(?i)\bhttps?://`},
	{"www_url", "url", `(?i)\bwww\.`},
	{"bare_domain", "url", `(?i)\b[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?\.(?:com|net|org|io|co|dev|app|invalid|example|xyz|info|biz)\b`},

	// exfiltration
	{"email_address", "exfiltration", `(?i)\b[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}\b`},
	{"send_export_data", "exfiltration", `(?i)\b(?:send|email|forward|export|upload|leak|exfiltrate)\b[^\n]{0,80}\b(?:ledger|export|balances?|transactions?)\b`},
	{"ledger_send_data", "exfiltration", `(?i)\b(?:ledger|export|balances?)\b[^\n]{0,80}\b(?:send|email|forward|upload|leak|exfiltrate)\b`},
}

// DefaultPatterns returns a fresh copy of the compiled default rule list.
func DefaultPatterns() []Pattern {
	patterns := make([]Pattern, 0, len(defaultPatternSpecs))
	for _, spec := range defaultPatternSpecs {
		patterns = append(patterns, Pattern{
			Name: spec.name,
			Code: spec.code,
			RE:   regexp.MustCompile(spec.expr),
		})
	}
	return patterns
}
