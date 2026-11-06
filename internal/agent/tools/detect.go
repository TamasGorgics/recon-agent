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
// names only string fields on that same object. A union of field names across
// the run is never used. The `raw` payload is attacker-controlled, so neither
// the metadata nor the values inside `raw` are scanned; only the record object
// that owns `raw` can authorize scanning of its own local fields.
//
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
		Data      json.RawMessage `json:"data"`
		Error     json.RawMessage `json:"error"`
		Truncated bool            `json:"truncated"`
	}
	if err := json.Unmarshal(envelope, &decoded); err != nil {
		slog.Debug("tools: injection detection skipped", "reason", "invalid envelope")
		return nil
	}
	if len(decoded.Error) > 0 && string(decoded.Error) != "null" {
		return nil
	}
	// Error, truncated and omitted envelopes contain no data to scan.
	if decoded.Truncated || len(decoded.Data) == 0 || string(decoded.Data) == "null" {
		return nil
	}
	var data any
	if err := json.Unmarshal(decoded.Data, &data); err != nil {
		slog.Debug("tools: injection detection skipped", "reason", "invalid data")
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

// walkUntrusted invokes scan for every string value named by an
// untrusted_fields array on the same object. Slices and objects are traversed
// recursively; a `raw` payload subtree is skipped entirely.
func walkUntrusted(node any, scan func(string)) {
	switch typed := node.(type) {
	case map[string]any:
		if raw, ok := typed["untrusted_fields"].([]any); ok {
			for _, item := range raw {
				field, ok := item.(string)
				if !ok {
					continue
				}
				value, ok := typed[field].(string)
				if !ok {
					continue
				}
				scan(value)
			}
		}
		for key, child := range typed {
			if key == "raw" {
				// Attacker-controlled payload; its keys are not authoritative
				// metadata and its values are only scanned when the owning
				// record marks a local field.
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
