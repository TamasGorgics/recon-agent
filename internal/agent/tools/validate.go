package tools

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// invalidJSONArgs is the sentinel recorded when the model sends syntactically
// invalid JSON. The raw bytes are never written to a JSONB column.
var invalidJSONArgs = json.RawMessage(`{"_invalid_json":true}`)

// integerLiteral matches the only JSON number spellings accepted for integer
// amount fields: an optional minus sign followed by digits with no fraction,
// exponent or leading zero. FIN-05 requires integer minor units, and JSON
// Schema alone treats 12.0 and 1e3 as integers.
var integerLiteral = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)

// normalizeArgs returns the JSON body to validate and record, and whether it
// was syntactically valid. Empty/nil arguments become an empty object so a
// tool with no required fields can run; tools with required fields still fail
// schema validation. Invalid JSON is replaced by the sentinel.
func normalizeArgs(args json.RawMessage) (json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(args)
	if len(trimmed) == 0 {
		return json.RawMessage(`{}`), true
	}
	if !json.Valid(trimmed) {
		return invalidJSONArgs, false
	}
	return json.RawMessage(trimmed), true
}

// validateArguments decodes args with json.Number precision, applies the
// compiled JSON Schema, then enforces the lexical integer rule for *_minor and
// explicitly integer-declared amount fields.
func validateArguments(schema *jsonschema.Schema, args json.RawMessage) error {
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(args))
	if err != nil {
		return wrapInvalidArguments("arguments are not valid JSON")
	}
	if err := schema.Validate(instance); err != nil {
		return wrapInvalidArguments("arguments do not match the tool schema")
	}
	if err := checkIntegerLexical(instance, schema); err != nil {
		return err
	}
	return nil
}

func checkIntegerLexical(value any, schema *jsonschema.Schema) error {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			var childSchema *jsonschema.Schema
			if schema != nil {
				childSchema = schema.Properties[key]
			}
			if strings.HasSuffix(key, "_minor") {
				number, ok := child.(json.Number)
				if !ok || !integerLiteral.MatchString(number.String()) {
					return wrapInvalidArguments("amount field %q must be an integer minor-unit value", key)
				}
			}
			if err := checkIntegerLexical(child, childSchema); err != nil {
				return err
			}
		}
	case []any:
		itemSchema := itemsSchema(schema)
		for _, child := range typed {
			if err := checkIntegerLexical(child, itemSchema); err != nil {
				return err
			}
		}
	case json.Number:
		if schemaAllowsInteger(schema) && !integerLiteral.MatchString(typed.String()) {
			return wrapInvalidArguments("integer amount fields must not use a fraction or exponent")
		}
	}
	return nil
}

func itemsSchema(schema *jsonschema.Schema) *jsonschema.Schema {
	if schema == nil {
		return nil
	}
	if schema.Items2020 != nil {
		return schema.Items2020
	}
	switch items := schema.Items.(type) {
	case *jsonschema.Schema:
		return items
	}
	return nil
}

func schemaAllowsInteger(schema *jsonschema.Schema) bool {
	if schema == nil || schema.Types == nil {
		return false
	}
	for _, name := range schema.Types.ToStrings() {
		if name == "integer" {
			return true
		}
	}
	return false
}
