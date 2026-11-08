package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// ExtractRefs decodes one persisted tool-call result envelope and returns its
// evidence references. Only successful, non-truncated envelopes contribute
// refs: an error envelope, a truncated envelope, or an omitted result returns
// an empty slice. Malformed JSON or an invalid reference is an error.
//
// The helper is the single ref-syntax boundary for the eval runner; callers
// must not re-implement ref parsing.
func ExtractRefs(result json.RawMessage) ([]string, error) {
	refs := make([]string, 0)
	if len(bytes.TrimSpace(result)) == 0 {
		return refs, nil
	}

	var envelope struct {
		Error     json.RawMessage `json:"error"`
		Refs      []string        `json:"refs"`
		Truncated bool            `json:"truncated"`
	}
	if err := json.Unmarshal(result, &envelope); err != nil {
		return nil, fmt.Errorf("%w: decode tool result envelope: %v", ErrInvalidRef, err)
	}
	if hasErrorObject(envelope.Error) || envelope.Truncated {
		return refs, nil
	}

	for _, ref := range envelope.Refs {
		if err := ValidateRef(ref); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// hasErrorObject reports whether a decoded error field is a JSON object. A
// missing field, null, or an empty raw message all mean the envelope carried
// no error.
func hasErrorObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return false
	}
	return true
}
