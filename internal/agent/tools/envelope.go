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

// untrustedFieldsFromData collects the sorted unique union of every
// per-record untrusted_fields array present in the returned data.
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
			if raw, ok := typed["untrusted_fields"]; ok {
				if list, ok := raw.([]any); ok {
					for _, item := range list {
						if field, ok := item.(string); ok {
							fields = append(fields, field)
						}
					}
				}
			}
			for _, child := range typed {
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
