package tools

import (
	"encoding/json"
	"log/slog"

	"github.com/TamasGorgics/recon-agent/internal/agent/untrusted"
)

// detectEnvelopeFlags walks a successful envelope's data and returns the
// sorted, unique injection-flag codes found in object-local untrusted fields.
//
// The walk is deliberately object-local: an object's untrusted_fields array
// names only fields declared by that same object. A union of field names across
// the run is never used. A declared name is resolved against the record object
// itself and against its own `raw` payload, and the resolved value is what gets
// scanned. The `raw` payload is attacker-controlled, so an `untrusted_fields`
// array inside it is never treated as metadata; it can only be scanned when the
// owning record declares the same name.
//
// Row-cap truncation keeps data and is still scanned. Only the size-cap
// envelope, which replaces data with an omission marker, has nothing to scan.
// Detection never changes the envelope and never returns matched text. A
// malformed envelope or a detector failure is a safe no-op.
func detectEnvelopeFlags(envelope []byte) (flags []string) {
	defer func() {
		if recover() != nil {
			slog.Debug("tools: injection detection skipped", "reason", "walker panic")
			flags = nil
		}
	}()

	var decoded struct {
		Data  json.RawMessage `json:"data"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(envelope, &decoded); err != nil {
		slog.Debug("tools: injection detection skipped", "reason", "invalid envelope")
		return nil
	}
	if len(decoded.Error) > 0 && string(decoded.Error) != "null" {
		return nil
	}
	if len(decoded.Data) == 0 || string(decoded.Data) == "null" {
		return nil
	}
	var data any
	if err := json.Unmarshal(decoded.Data, &data); err != nil {
		slog.Debug("tools: injection detection skipped", "reason", "invalid data")
		return nil
	}
	// The size-cap envelope contains no evidence: its data is only the
	// omission marker. Every other result, including a row-cap-truncated one,
	// carries real data and is scanned.
	if isOmittedData(data) {
		return nil
	}

	var codes []string
	walkUntrusted(data, func(value string) {
		for _, flag := range untrusted.Suspicious(untrusted.Sanitize(value)) {
			codes = append(codes, flag.Code)
		}
	})
	return SortedUnique(codes)
}

// isOmittedData reports whether data is the size-cap omission marker
// ({"omitted":true,...}) rather than tool output.
func isOmittedData(data any) bool {
	object, ok := data.(map[string]any)
	if !ok {
		return false
	}
	omitted, ok := object["omitted"].(bool)
	return ok && omitted
}

// walkUntrusted invokes scan for every string value named by an object-local
// untrusted_fields array. A name is resolved against the owning object's own
// fields and its own `raw` payload, so real read-tool records whose text lives
// under `raw` are scanned. The `raw` subtree is otherwise skipped: its keys are
// never authoritative metadata.
func walkUntrusted(node any, scan func(string)) {
	switch typed := node.(type) {
	case map[string]any:
		if declared, ok := typed["untrusted_fields"].([]any); ok {
			raw, _ := typed["raw"].(map[string]any)
			for _, item := range declared {
				field, ok := item.(string)
				if !ok {
					continue
				}
				if value, ok := typed[field].(string); ok {
					scan(value)
				}
				if raw != nil {
					if value, ok := raw[field].(string); ok {
						scan(value)
					}
				}
			}
		}
		for key, child := range typed {
			if key == "raw" || key == "untrusted_fields" {
				// raw is attacker-controlled payload; untrusted_fields is the
				// declaration list, not content.
				continue
			}
			walkUntrusted(child, scan)
		}
	case []any:
		for _, child := range typed {
			walkUntrusted(child, scan)
		}
	}
}
