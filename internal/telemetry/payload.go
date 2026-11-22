package telemetry

import (
	"crypto/sha256"
	"fmt"

	"go.opentelemetry.io/otel/attribute"

	"github.com/TamasGorgics/recon-agent/internal/llm"
)

// DefaultPayloadLimit is the number of redacted payload bytes retained before
// the truncation marker.
const DefaultPayloadLimit = 2 << 10 // 2 KiB

// SafePayload redacts the payload with the LLM redactor, then truncates at a
// valid UTF-8 boundary. The truncation marker carries SHA-256 of the original
// raw bytes and is not counted against DefaultPayloadLimit. Redaction happens
// before truncation, so a secret crossing the boundary cannot leak a prefix.
// It is deterministic and never returns more than the limit plus the marker.
func SafePayload(raw []byte) string {
	redacted := llm.Redact(string(raw))
	if len(redacted) <= DefaultPayloadLimit {
		return redacted
	}
	sum := sha256.Sum256(raw)
	return truncateUTF8(redacted, DefaultPayloadLimit) +
		fmt.Sprintf("...[truncated sha256=%x]", sum)
}

// SafePayloadAttribute returns key=SafePayload(raw) only when payload capture
// is explicitly enabled. With capture off (the default) no span attribute is
// produced; instrumentation should record hashes and counts instead.
func SafePayloadAttribute(key attribute.Key, raw []byte) (attribute.KeyValue, bool) {
	if !CapturePayloadsEnabled() {
		return attribute.KeyValue{}, false
	}
	return attribute.String(string(key), SafePayload(raw)), true
}
