package metrics

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// unknownLabelValue is the only fallback for an unrecognized label value. Every
// open-ended allow-list accepts it, and closed internal enums never expose a
// method that can put an arbitrary string into their label.
const unknownLabelValue = "unknown"

// maxLabelValueLen bounds every label value before it reaches a collector. The
// longest catalogued validator is model (64), so 128 leaves headroom without
// allowing free text to inflate scrape size.
const maxLabelValueLen = 128

// Forbidden label names: these identify a row or carry free text instead of a
// bounded dimension. The *_id suffix rule below also catches entity IDs that
// are not listed explicitly.
var forbiddenLabelNames = map[string]struct{}{
	"id":              {},
	"case":            {},
	"case_id":         {},
	"run":             {},
	"run_id":          {},
	"agent_run_id":    {},
	"proposal":        {},
	"proposal_id":     {},
	"break":           {},
	"break_id":        {},
	"scope":           {},
	"scope_id":        {},
	"ledger":          {},
	"ledger_txn_id":   {},
	"ledger_id":       {},
	"approver":        {},
	"approver_id":     {},
	"trace":           {},
	"trace_id":        {},
	"match":           {},
	"match_id":        {},
	"outbox":          {},
	"outbox_id":       {},
	"idempotency":     {},
	"idempotency_key": {},
	"idempotency_id":  {},
	"entity_id":       {},
	"message_id":      {},
	"external_ref":    {},
	"uuid":            {},
	"guid":            {},
	"payload":         {},
	"error":           {},
	"reason":          {},
	"rationale":       {},
	"detail":          {},
	"description":     {},
	"body":            {},
	"raw":             {},
	"text":            {},
	"dsn":             {},
	"url":             {},
	"path":            {},
	"email":           {},
}

var (
	metricNameRE = regexp.MustCompile(`^recon_[a-z0-9_]+$`)
	labelNameRE  = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

	sourceValueRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_]{0,31}$`)
	currencyRE    = regexp.MustCompile(`^[A-Z]{3}$`)
	modelValueRE  = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,64}$`)

	uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	ulidRE = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)
	hexRE  = regexp.MustCompile(`^[0-9a-fA-F]{16,}$`)
)

// isForbiddenLabelName reports whether a label name is an identifier or
// free-text label forbidden by the cardinality contract.
func isForbiddenLabelName(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	if _, ok := forbiddenLabelNames[name]; ok {
		return true
	}
	if strings.HasSuffix(name, "_id") {
		return true
	}
	return strings.HasSuffix(name, "_uuid") || strings.HasSuffix(name, "_guid")
}

// validateLabelName checks the syntactic contract for every label in a
// registered family. le and quantile are reserved by client_golang for
// histogram and summary output.
func validateLabelName(name string) error {
	switch {
	case name == "":
		return errLabelNameEmpty
	case len(name) > 64:
		return errLabelNameLong
	case !labelNameRE.MatchString(name):
		return errLabelNameSyntax
	case name == "le" || name == "quantile":
		return errLabelNameReserved
	case isForbiddenLabelName(name):
		return errLabelForbidden
	default:
		return nil
	}
}

// normalizeEnum returns the value when it is in the closed allow-list and
// unknown otherwise. Allow-lists that intentionally expose no unknown child
// (queue and build component) are validated strictly by their callers and are
// never passed to this helper with untrusted input.
func normalizeEnum(value string, allowed []string) string {
	if slices.Contains(allowed, value) {
		return value
	}
	return unknownLabelValue
}

func normalizeSource(value string) string {
	if value == unknownLabelValue {
		return unknownLabelValue
	}
	if !sourceValueRE.MatchString(value) || looksLikeIdentifier(value) {
		return unknownLabelValue
	}
	return value
}

func normalizeCurrency(value string) string {
	if value == unknownLabelValue {
		return unknownLabelValue
	}
	if !currencyRE.MatchString(value) {
		return unknownLabelValue
	}
	return value
}

func normalizeModel(value string) string {
	if value == unknownLabelValue {
		return unknownLabelValue
	}
	if !modelValueRE.MatchString(value) ||
		strings.Contains(value, "://") ||
		looksLikeIdentifier(value) {
		return unknownLabelValue
	}
	return value
}

func normalizeRule(value string) string   { return normalizeEnum(value, ruleValues) }
func normalizeSide(value string) string   { return normalizeEnum(value, sideValues) }
func normalizeStatus(value string) string { return normalizeEnum(value, statusValues) }
func normalizeOutcome(value string) string {
	return normalizeEnum(value, outcomeValues)
}
func normalizeTier(value string) string { return normalizeEnum(value, tierValues) }
func normalizeTopic(value string) string {
	return normalizeEnum(value, topicValues)
}
func normalizeComponent(value string) string {
	return normalizeEnum(value, componentValues)
}
func normalizeCode(value string) string { return normalizeEnum(value, codeValues) }

// normalizeQueue is strict: queue is a closed internal enum with no unknown
// child, so an unrecognized value is rejected rather than silently seeded.
func normalizeQueue(value string) (string, bool) {
	if slices.Contains(queueValues, value) {
		return value, true
	}
	return "", false
}

// looksLikeIdentifier rejects UUID, ULID, and long hexadecimal strings so a
// row identifier can never become a source, model, or tie-out series even when
// it happens to satisfy the charset validator.
func looksLikeIdentifier(value string) bool {
	return uuidRE.MatchString(value) || ulidRE.MatchString(value) || hexRE.MatchString(value)
}

// safeLabelValue applies checks shared by every label value: no empty or
// padded value, no control characters, no URL, no identifier, and a bounded
// length.
func safeLabelValue(value string) bool {
	if value == "" || len(value) > maxLabelValueLen {
		return false
	}
	if strings.TrimSpace(value) != value {
		return false
	}
	if strings.Contains(value, "://") || looksLikeIdentifier(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// validateLabelValue checks a value against one catalogued label spec. Enum
// values must match exactly; open-ended validators must normalize to
// themselves, which proves no fallback changed the value.
func validateLabelValue(spec LabelSpec, value string) bool {
	if !safeLabelValue(value) {
		return false
	}
	switch spec.Kind {
	case LabelEnum:
		return slices.Contains(spec.Allowed, value)
	case LabelSource:
		return normalizeSource(value) == value
	case LabelCurrency:
		return normalizeCurrency(value) == value
	case LabelModel:
		return normalizeModel(value) == value
	default:
		return false
	}
}

var (
	errLabelNameEmpty    = &labelSpecError{"label name is empty"}
	errLabelNameLong     = &labelSpecError{"label name exceeds 64 characters"}
	errLabelNameSyntax   = &labelSpecError{"label name must match ^[a-z][a-z0-9_]*$"}
	errLabelNameReserved = &labelSpecError{"label name is reserved by Prometheus"}
	errLabelForbidden    = &labelSpecError{"label name is an identifier or free-text label"}
)

type labelSpecError struct{ msg string }

func (e *labelSpecError) Error() string { return e.msg }
