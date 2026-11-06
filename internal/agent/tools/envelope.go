package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

const truncationReason = "result_too_large"

type errorObject struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type successEnvelope struct {
	Data            json.RawMessage `json:"data"`
	Refs            []string        `json:"refs"`
	UntrustedFields []string        `json:"untrusted_fields"`
	Truncated       bool            `json:"truncated"`
}

type errorEnvelope struct {
	Error           errorObject `json:"error"`
	Refs            []string    `json:"refs"`
	UntrustedFields []string    `json:"untrusted_fields"`
	Truncated       bool        `json:"truncated"`
}

func emptyStrings() []string { return []string{} }

func marshalErrorEnvelope(code, message string) []byte {
	data, err := json.Marshal(errorEnvelope{
		Error:           errorObject{Code: code, Message: message},
		Refs:            emptyStrings(),
		UntrustedFields: emptyStrings(),
	})
	if err != nil {
		// The error envelope contains only strings and empty arrays. A
		// marshal failure here would be a programming error, but we still
		// return a valid fallback rather than an invalid payload.
		return []byte(`{"error":{"code":"internal_error","message":"internal error"},"refs":[],"untrusted_fields":[],"truncated":false}`)
	}
	return data
}

func marshalTruncatedEnvelope() []byte {
	data, err := json.Marshal(successEnvelope{
		Data:            json.RawMessage(`{"omitted":true,"reason":"` + truncationReason + `"}`),
		Refs:            emptyStrings(),
		UntrustedFields: emptyStrings(),
		Truncated:       true,
	})
	if err != nil {
		return []byte(`{"data":{"omitted":true,"reason":"result_too_large"},"refs":[],"untrusted_fields":[],"truncated":true}`)
	}
	return data
}

// buildSuccessEnvelope serializes a handler result, normalizes refs, derives
// the untrusted-field union from the data, and applies the size cap. It returns
// the final envelope and the refs that may be added to the run context. When
// the cap is exceeded, the returned refs are nil and the envelope carries no
// evidence.
func buildSuccessEnvelope(result Result, maxBytes int) ([]byte, []string, error) {
	data, err := json.Marshal(result.Data)
	if err != nil {
		return nil, nil, err
	}
	refs, err := normalizeRefs(result.Refs)
	if err != nil {
		return nil, nil, err
	}
	envelope, err := json.Marshal(successEnvelope{
		Data:            data,
		Refs:            refs,
		UntrustedFields: untrustedFieldsFromData(data),
		Truncated:       result.Truncated,
	})
	if err != nil {
		return nil, nil, err
	}
	if maxBytes > 0 && len(envelope) > maxBytes {
		return marshalTruncatedEnvelope(), nil, nil
	}
	return envelope, refs, nil
}

// normalizeRefs validates every evidence reference, sorts them and removes
// duplicates. The returned slice is never nil.
func normalizeRefs(refs []string) ([]string, error) {
	if len(refs) == 0 {
		return emptyStrings(), nil
	}
	for _, ref := range refs {
		if err := ValidateRef(ref); err != nil {
			return nil, err
		}
	}
	return SortedUnique(refs), nil
}

// untrustedFieldsFromData collects the sorted unique union of the per-record
// untrusted_fields declarations present in the returned data. A declared name
// is advertised only when it resolves to a field on the record object or in the
// record's own raw payload. The raw payload is never itself treated as
// metadata, so an attacker-supplied raw.untrusted_fields array contributes no
// names.
func untrustedFieldsFromData(data []byte) []string {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return emptyStrings()
	}
	var fields []string
	var walk func(any)
	walk = func(node any) {
		switch typed := node.(type) {
		case map[string]any:
			raw, _ := typed["raw"].(map[string]any)
			if declared, ok := typed["untrusted_fields"].([]any); ok {
				for _, item := range declared {
					field, ok := item.(string)
					if !ok || field == "" {
						continue
					}
					if _, present := typed[field]; present {
						fields = append(fields, field)
						continue
					}
					if raw != nil {
						if _, present := raw[field]; present {
							fields = append(fields, field)
						}
					}
				}
			}
			for key, child := range typed {
				if key == "raw" || key == "untrusted_fields" {
					continue
				}
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(value)
	return SortedUnique(fields)
}

func hashResult(envelope []byte) string {
	sum := sha256.Sum256(envelope)
	return hex.EncodeToString(sum[:])
}
