package score

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
	"github.com/TamasGorgics/recon-agent/internal/money"
)

var (
	uuidPattern   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	sourcePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	controlChars  = regexp.MustCompile(`[\x00-\x1f\x7f]`)
)

// parsedProposal is the canonical decoded form of one proposal payload. Only
// the fields relevant to the proposal's own type are populated.
type parsedProposal struct {
	kind      string
	link      *linkValue
	lines     []adjustmentLine
	duplicate *duplicateValue
	question  string
}

type linkValue struct {
	ledgerTxnIDs []string // canonical lowercase, sorted unique
	externalRef  string   // canonical ext:<source>:<record_id>
}

type adjustmentLine struct {
	accountCode string
	direction   string
	amountMinor int64
	currency    string
}

type duplicateValue struct {
	externalRef string
	duplicateOf string // empty means the payload omitted duplicate_of
}

// parseProposal decodes one proposal payload strictly. A non-empty error code
// means the actual payload is malformed; callers turn that into an incorrect
// case result rather than a hard error.
func parseProposal(proposal Proposal) (*parsedProposal, string) {
	switch proposal.Type {
	case "link":
		value, err := parseLink(proposal.Payload)
		if err != nil {
			return nil, ErrorInvalidProposalPayload
		}
		return &parsedProposal{kind: "link", link: value}, ""
	case "adjustment", "write_off":
		lines, err := parseAdjustmentLines(proposal.Payload)
		if err != nil {
			return nil, ErrorInvalidProposalPayload
		}
		return &parsedProposal{kind: proposal.Type, lines: lines}, ""
	case "duplicate":
		value, err := parseDuplicate(proposal.Payload)
		if err != nil {
			return nil, ErrorInvalidProposalPayload
		}
		return &parsedProposal{kind: "duplicate", duplicate: value}, ""
	case "request_info":
		question, err := parseRequestInfo(proposal.Payload)
		if err != nil {
			return nil, ErrorInvalidProposalPayload
		}
		return &parsedProposal{kind: "request_info", question: question}, ""
	default:
		return nil, ErrorUnknownProposalType
	}
}

func decodeStrict(data json.RawMessage, dst any) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("payload is not a JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("payload has trailing JSON values")
		}
		return err
	}
	return nil
}

type linkPayload struct {
	LedgerTxnID  *string   `json:"ledger_txn_id"`
	LedgerTxnIDs *[]string `json:"ledger_txn_ids"`
	ExternalRef  *string   `json:"external_ref"`
}

func parseLink(data json.RawMessage) (*linkValue, error) {
	var payload linkPayload
	if err := decodeStrict(data, &payload); err != nil {
		return nil, err
	}
	if (payload.LedgerTxnID == nil) == (payload.LedgerTxnIDs == nil) {
		return nil, errors.New("exactly one of ledger_txn_id or ledger_txn_ids is required")
	}
	if payload.ExternalRef == nil {
		return nil, errors.New("external_ref is required")
	}
	externalRef, ok := canonicalExternalRef(*payload.ExternalRef)
	if !ok {
		return nil, errors.New("external_ref is not a canonical ext reference")
	}

	var ids []string
	if payload.LedgerTxnID != nil {
		id, ok := canonicalUUID(*payload.LedgerTxnID)
		if !ok {
			return nil, errors.New("ledger_txn_id is not a UUID")
		}
		ids = []string{id}
	} else {
		if len(*payload.LedgerTxnIDs) == 0 {
			return nil, errors.New("ledger_txn_ids is empty")
		}
		seen := make(map[string]struct{}, len(*payload.LedgerTxnIDs))
		ids = make([]string, 0, len(*payload.LedgerTxnIDs))
		for _, raw := range *payload.LedgerTxnIDs {
			id, ok := canonicalUUID(raw)
			if !ok {
				return nil, errors.New("ledger_txn_ids contains a non-UUID")
			}
			if _, duplicate := seen[id]; duplicate {
				return nil, errors.New("ledger_txn_ids contains a duplicate")
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return &linkValue{ledgerTxnIDs: ids, externalRef: externalRef}, nil
}

type adjustmentPayload struct {
	Lines []adjustmentLinePayload `json:"lines"`
}

type adjustmentLinePayload struct {
	AccountCode string `json:"account_code"`
	Direction   string `json:"direction"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

func parseAdjustmentLines(data json.RawMessage) ([]adjustmentLine, error) {
	var payload adjustmentPayload
	if err := decodeStrict(data, &payload); err != nil {
		return nil, err
	}
	if len(payload.Lines) == 0 {
		return nil, errors.New("lines is empty")
	}
	lines := make([]adjustmentLine, 0, len(payload.Lines))
	for index, line := range payload.Lines {
		if line.AccountCode == "" {
			return nil, fmt.Errorf("line %d: account_code is empty", index)
		}
		if line.Direction != "debit" && line.Direction != "credit" {
			return nil, fmt.Errorf("line %d: direction %q is invalid", index, line.Direction)
		}
		if line.AmountMinor <= 0 {
			return nil, fmt.Errorf("line %d: amount_minor must be a positive integer", index)
		}
		if _, err := money.ParseCurrency(line.Currency); err != nil {
			return nil, fmt.Errorf("line %d: %w", index, err)
		}
		lines = append(lines, adjustmentLine{
			accountCode: line.AccountCode,
			direction:   line.Direction,
			amountMinor: line.AmountMinor,
			currency:    line.Currency,
		})
	}
	return lines, nil
}

type duplicatePayload struct {
	ExternalRef string  `json:"external_ref"`
	DuplicateOf *string `json:"duplicate_of"`
}

func parseDuplicate(data json.RawMessage) (*duplicateValue, error) {
	var payload duplicatePayload
	if err := decodeStrict(data, &payload); err != nil {
		return nil, err
	}
	externalRef, ok := canonicalExternalRef(payload.ExternalRef)
	if !ok {
		return nil, errors.New("external_ref is not a canonical ext reference")
	}
	duplicateOf := ""
	if payload.DuplicateOf != nil {
		var valid bool
		duplicateOf, valid = canonicalExternalRef(*payload.DuplicateOf)
		if !valid {
			return nil, errors.New("duplicate_of is not a canonical ext reference")
		}
		if duplicateOf == externalRef {
			return nil, errors.New("duplicate_of must differ from external_ref")
		}
	}
	return &duplicateValue{externalRef: externalRef, duplicateOf: duplicateOf}, nil
}

type requestInfoPayload struct {
	Question string `json:"question"`
}

func parseRequestInfo(data json.RawMessage) (string, error) {
	var payload requestInfoPayload
	if err := decodeStrict(data, &payload); err != nil {
		return "", err
	}
	if strings.TrimSpace(payload.Question) == "" {
		return "", errors.New("question is empty")
	}
	return payload.Question, nil
}

// equivalent compares a resolution that is already known to be in the truth
// allowed set. It returns false when the payload does not match the truth
// effect. Write-off has no expected payload in truth v1, so its equivalence is
// allowed-set membership only (documented in docs/eval-scoring.md).
func equivalent(truth dataset.ScenarioTruth, resolution string, parsed *parsedProposal) bool {
	switch resolution {
	case "escalate":
		return true
	case "request_info":
		return parsed != nil && parsed.kind == "request_info"
	case "link":
		return parsed != nil && parsed.link != nil && truth.ExpectedLink != nil &&
			linkSetEqual(truth.ExpectedLink, parsed.link)
	case "adjustment":
		return parsed != nil && parsed.kind == "adjustment" &&
			truth.ExpectedAdjustment != nil &&
			adjustmentSetEqual(parsed.lines, truth.ExpectedAdjustment.Lines)
	case "write_off":
		return parsed != nil && parsed.kind == "write_off"
	case "duplicate":
		return parsed != nil && parsed.duplicate != nil && truth.ExpectedDuplicate != nil &&
			duplicateEqual(truth.ExpectedDuplicate, parsed.duplicate)
	default:
		return false
	}
}

func linkSetEqual(expected *dataset.ExpectedLink, actual *linkValue) bool {
	expectedSet := make(map[string]struct{}, len(expected.LedgerTxnIDs)+len(expected.ExternalRecords))
	for _, id := range expected.LedgerTxnIDs {
		expectedSet["ledger_txn:"+strings.ToLower(id)] = struct{}{}
	}
	for _, record := range expected.ExternalRecords {
		expectedSet["ext:"+record.Source+":"+record.SourceRecordID] = struct{}{}
	}
	actualSet := make(map[string]struct{}, len(actual.ledgerTxnIDs)+1)
	for _, id := range actual.ledgerTxnIDs {
		actualSet["ledger_txn:"+id] = struct{}{}
	}
	actualSet[actual.externalRef] = struct{}{}
	return equalStringSets(actualSet, expectedSet)
}

func adjustmentSetEqual(actual []adjustmentLine, expected []dataset.ExpectedLine) bool {
	actualCounts := make(map[string]int, len(actual))
	for _, line := range actual {
		actualCounts[adjustmentKey(line.accountCode, line.direction, line.amountMinor, line.currency)]++
	}
	expectedCounts := make(map[string]int, len(expected))
	for _, line := range expected {
		expectedCounts[adjustmentKey(line.AccountCode, line.Direction, line.AmountMinor, line.Currency)]++
	}
	if len(actualCounts) != len(expectedCounts) {
		return false
	}
	for key, count := range expectedCounts {
		if actualCounts[key] != count {
			return false
		}
	}
	return true
}

func duplicateEqual(expected *dataset.ExpectedDuplicate, actual *duplicateValue) bool {
	if actual.externalRef != expected.ExternalRef {
		return false
	}
	// Spec 6.2.5: an omitted duplicate_of is accepted when truth names the
	// original, but any supplied value must match.
	if expected.DuplicateOf != "" && actual.duplicateOf != "" &&
		actual.duplicateOf != expected.DuplicateOf {
		return false
	}
	// When truth deliberately leaves duplicate_of empty, the actual must omit
	// it as well.
	if expected.DuplicateOf == "" && actual.duplicateOf != "" {
		return false
	}
	return true
}

func adjustmentKey(accountCode, direction string, amountMinor int64, currency string) string {
	return accountCode + "\x00" + direction + "\x00" +
		strconv.FormatInt(amountMinor, 10) + "\x00" + currency
}

// falseFix implements spec 6.3: only adjustment/write_off proposals can be
// false fixes, because only they affect the ledger. A matching expected
// adjustment (including the partial-refund adjustment path) is not a false fix;
// a write_off explicitly allowed by truth is not one either. Every other
// adjustment/write_off effect differs from truth and is a false fix.
func falseFix(truth dataset.ScenarioTruth, proposal *Proposal, parsed *parsedProposal) bool {
	if proposal == nil || (proposal.Type != "adjustment" && proposal.Type != "write_off") {
		return false
	}
	if proposal.Type == "write_off" && contains(truth.ExpectedResolutions, "write_off") {
		return false
	}
	if proposal.Type == "adjustment" &&
		contains(truth.ExpectedResolutions, "adjustment") &&
		truth.ExpectedAdjustment != nil &&
		parsed != nil && parsed.kind == "adjustment" &&
		adjustmentSetEqual(parsed.lines, truth.ExpectedAdjustment.Lines) {
		return false
	}
	return true
}

func canonicalUUID(id string) (string, bool) {
	if !uuidPattern.MatchString(id) {
		return "", false
	}
	return strings.ToLower(id), true
}

func canonicalExternalRef(ref string) (string, bool) {
	if !strings.HasPrefix(ref, "ext:") {
		return "", false
	}
	rest := strings.TrimPrefix(ref, "ext:")
	source, recordID, ok := strings.Cut(rest, ":")
	if !ok || !sourcePattern.MatchString(source) || recordID == "" || controlChars.MatchString(recordID) {
		return "", false
	}
	return "ext:" + source + ":" + recordID, true
}

func equalStringSets(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for value := range a {
		if _, ok := b[value]; !ok {
			return false
		}
	}
	return true
}
