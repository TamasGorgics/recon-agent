package proposal

import (
	"encoding/json"
	"fmt"
)

const uuidSchemaPattern = `^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`

var (
	schemaLink         = buildProposalSchema(linkProperties(), []string{"ledger_txn_id", "external_ref"}, true)
	schemaAdjustment   = buildProposalSchema(linesProperties(), []string{"lines"}, true)
	schemaWriteOff     = buildProposalSchema(linesProperties(), []string{"lines"}, true)
	schemaDuplicate    = buildProposalSchema(duplicateProperties(), []string{"external_ref"}, false)
	schemaRequestInfo  = buildProposalSchema(requestInfoProperties(), []string{"question"}, false)
	schemaEscalate     = buildEscalateSchema()
	patternEnumValues  = []string{"fee_variance", "fx_rounding", "rounding", "reference_typo", "other"}
	confidenceSchema   = map[string]any{"type": "number", "minimum": 0, "maximum": 1}
	caseIDSchema       = map[string]any{"type": "string", "pattern": uuidSchemaPattern}
	rationaleSchema    = map[string]any{"type": "string", "minLength": 1, "maxLength": 2000}
	evidenceSchema     = map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "string"}, "description": "canonical refs returned by successful tool calls in this run"}
	externalRefSchema  = map[string]any{"type": "string", "minLength": 1, "description": "canonical external evidence reference ext:<source>:<source_record_id>"}
	ledgerTxnIDSchema  = map[string]any{"type": "string", "pattern": uuidSchemaPattern}
	accountCodeSchema  = map[string]any{"type": "string", "minLength": 1}
	directionSchema    = map[string]any{"type": "string", "enum": []string{"debit", "credit"}}
	amountMinorSchema  = map[string]any{"type": "integer", "minimum": 1}
	currencySchema     = map[string]any{"type": "string", "pattern": "^[A-Z]{3}$"}
	questionSchema     = map[string]any{"type": "string", "minLength": 1, "maxLength": 2000}
	reasonSchema       = map[string]any{"type": "string", "minLength": 1, "maxLength": 500}
	summarySchema      = map[string]any{"type": "string", "minLength": 1, "maxLength": 2000}
	lineItemProperties = map[string]any{
		"account_code": accountCodeSchema,
		"direction":    directionSchema,
		"amount_minor": amountMinorSchema,
		"currency":     currencySchema,
	}
	lineItemRequired = []string{"account_code", "direction", "amount_minor", "currency"}
)

func commonProperties() map[string]any {
	return map[string]any{
		"case_id":    caseIDSchema,
		"rationale":  rationaleSchema,
		"evidence":   evidenceSchema,
		"confidence": confidenceSchema,
	}
}

func linkProperties() map[string]any {
	properties := commonProperties()
	properties["ledger_txn_id"] = ledgerTxnIDSchema
	properties["external_ref"] = externalRefSchema
	return properties
}

func linesProperties() map[string]any {
	properties := commonProperties()
	properties["lines"] = map[string]any{
		"type":     "array",
		"minItems": 2,
		"items": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             lineItemRequired,
			"properties":           lineItemProperties,
		},
	}
	return properties
}

func duplicateProperties() map[string]any {
	properties := commonProperties()
	properties["external_ref"] = externalRefSchema
	properties["duplicate_of"] = externalRefSchema
	return properties
}

func requestInfoProperties() map[string]any {
	properties := commonProperties()
	properties["question"] = questionSchema
	return properties
}

func buildProposalSchema(properties map[string]any, required []string, allowPattern bool) json.RawMessage {
	if allowPattern {
		properties["pattern"] = map[string]any{"type": "string", "enum": patternEnumValues}
	}
	base := []string{"case_id", "rationale", "evidence", "confidence"}
	return mustMarshal(map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             append(base, required...),
		"properties":           properties,
	})
}

func buildEscalateSchema() json.RawMessage {
	return mustMarshal(map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"case_id", "reason", "summary"},
		"properties": map[string]any{
			"case_id": caseIDSchema,
			"reason":  reasonSchema,
			"summary": summarySchema,
		},
	})
}

func mustMarshal(value any) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("proposal: tool schema: %v", err))
	}
	return raw
}
