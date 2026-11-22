package proposal

import (
	"encoding/json"
	"strings"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
)

// Fields is the database-independent projection of one proposal. It is the
// exact set the proposal digest covers, and the shape replay rebuilds from the
// recorded proposal tool-call arguments.
type Fields struct {
	Type           string
	Payload        map[string]any
	Rationale      string
	Evidence       []string
	Confidence     string
	PatternClaimed *string
}

// Pattern returns the claimed pattern as a plain string. A nil pointer is the
// SQL NULL value the recorder stores for an absent or empty pattern.
func (f Fields) Pattern() string {
	if f.PatternClaimed == nil {
		return ""
	}
	return *f.PatternClaimed
}

// Rebuild decodes the arguments of one proposal tool call and applies the same
// pure normalization the recording pipeline applies before any database check
// (record steps 2 through 5). It never reads or writes a database.
//
// caseID is the run's case id. The arguments must name the same case; a
// mismatch is rejected exactly as the production tool rejects it.
func Rebuild(kind string, args json.RawMessage, caseID string) (Fields, error) {
	req, err := decodeRecordRequest(kind, args)
	if err != nil {
		return Fields{}, err
	}
	return normalizeRecordRequest(req, caseID)
}

// normalizeRecordRequest is the pure normalization shared with Rebuild. It is
// deliberately separate from the DB-checking pipeline so replay can rebuild a
// proposal without a pool.
func normalizeRecordRequest(req recordRequest, runCaseID string) (Fields, error) {
	runCase, err := tools.ParseUUID(runCaseID)
	if err != nil {
		return Fields{}, caseMismatchError()
	}
	runCase = strings.ToLower(runCase)
	caseID, err := tools.ParseUUID(req.caseID)
	if err != nil {
		return Fields{}, invalidArgumentsError("case_id must be a UUID")
	}
	caseID = strings.ToLower(caseID)
	if caseID != runCase {
		return Fields{}, caseMismatchError()
	}
	req.caseID = caseID

	rationale := sanitizeRequired(req.rationale)
	if rationale == "" {
		return Fields{}, invalidArgumentsError("rationale is empty after sanitization")
	}
	if req.pattern != "" && req.kind != "link" && req.kind != "adjustment" && req.kind != "write_off" {
		return Fields{}, invalidArgumentsError("pattern is not allowed for this tool")
	}
	if req.kind == "link" {
		if req.linkSet {
			ids, err := normalizeLedgerTxnIDs(req.ledgerTxnIDs)
			if err != nil {
				return Fields{}, err
			}
			req.ledgerTxnIDs = ids
		} else {
			ledgerTxnID, err := tools.ParseUUID(req.ledgerTxnID)
			if err != nil {
				return Fields{}, invalidArgumentsError("ledger_txn_id must be a UUID")
			}
			req.ledgerTxnID = strings.ToLower(ledgerTxnID)
		}
	}
	_, confidenceText, err := parseConfidence(req.confidence)
	if err != nil {
		return Fields{}, invalidArgumentsError("confidence must be between 0 and 1")
	}
	if req.kind == "request_info" {
		req.question = sanitizeRequired(req.question)
	}
	refs, err := canonicalEvidence(req.evidence)
	if err != nil {
		return Fields{}, err
	}
	payload, err := buildPayload(req)
	if err != nil {
		return Fields{}, &tools.CodedError{Code: tools.CodeInternalError, Message: "could not build proposal payload"}
	}
	var pattern *string
	if req.pattern != "" {
		claimed := req.pattern
		pattern = &claimed
	}
	return Fields{
		Type:           req.kind,
		Payload:        payload,
		Rationale:      rationale,
		Evidence:       refs,
		Confidence:     confidenceText,
		PatternClaimed: pattern,
	}, nil
}

// decodeRecordRequest decodes one proposal tool's arguments into the shared
// request shape. It is the single decoder used by both the production tools and
// the replay rebuild path, so the two cannot drift.
func decodeRecordRequest(kind string, args json.RawMessage) (recordRequest, error) {
	switch kind {
	case "link":
		var request struct {
			commonArgs
			LedgerTxnID  *string   `json:"ledger_txn_id"`
			LedgerTxnIDs *[]string `json:"ledger_txn_ids"`
			ExternalRef  string    `json:"external_ref"`
		}
		if err := decodeArguments(args, &request); err != nil {
			return recordRequest{}, err
		}
		if (request.LedgerTxnID == nil) == (request.LedgerTxnIDs == nil) {
			return recordRequest{}, invalidArgumentsError("exactly one of ledger_txn_id or ledger_txn_ids is required")
		}
		record := recordRequest{
			kind:        "link",
			caseID:      request.CaseID,
			rationale:   request.Rationale,
			evidence:    request.Evidence,
			confidence:  request.Confidence,
			pattern:     request.pattern(),
			externalRef: request.ExternalRef,
		}
		if request.LedgerTxnID != nil {
			record.ledgerTxnID = *request.LedgerTxnID
		} else {
			record.ledgerTxnIDs = *request.LedgerTxnIDs
			record.linkSet = true
		}
		return record, nil
	case "adjustment", "write_off":
		var request struct {
			commonArgs
			Lines []lineInput `json:"lines"`
		}
		if err := decodeArguments(args, &request); err != nil {
			return recordRequest{}, err
		}
		return recordRequest{
			kind:       kind,
			caseID:     request.CaseID,
			rationale:  request.Rationale,
			evidence:   request.Evidence,
			confidence: request.Confidence,
			pattern:    request.pattern(),
			lines:      request.Lines,
		}, nil
	case "duplicate":
		var request struct {
			commonArgs
			ExternalRef string  `json:"external_ref"`
			DuplicateOf *string `json:"duplicate_of"`
		}
		if err := decodeArguments(args, &request); err != nil {
			return recordRequest{}, err
		}
		duplicateOf := ""
		if request.DuplicateOf != nil {
			duplicateOf = *request.DuplicateOf
		}
		return recordRequest{
			kind:        "duplicate",
			caseID:      request.CaseID,
			rationale:   request.Rationale,
			evidence:    request.Evidence,
			confidence:  request.Confidence,
			pattern:     request.pattern(),
			externalRef: request.ExternalRef,
			duplicateOf: duplicateOf,
		}, nil
	case "request_info":
		var request struct {
			commonArgs
			Question string `json:"question"`
		}
		if err := decodeArguments(args, &request); err != nil {
			return recordRequest{}, err
		}
		return recordRequest{
			kind:       "request_info",
			caseID:     request.CaseID,
			rationale:  request.Rationale,
			evidence:   request.Evidence,
			confidence: request.Confidence,
			question:   request.Question,
		}, nil
	default:
		return recordRequest{}, &tools.CodedError{Code: tools.CodeInternalError, Message: "unknown proposal type"}
	}
}
