package telemetry

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"

	"github.com/TamasGorgics/recon-agent/internal/llm"
)

func TestSafePayloadRedactsSecretClasses(t *testing.T) {
	raw := []byte(`{"api_key":"oc_sk_live_123","authorization":"Bearer topsecret",` +
		`"session":"ses_abc123","memo":"call me"}`)

	got := SafePayload(raw)
	for _, secret := range []string{"oc_sk_live_123", "topsecret", "ses_abc123", "Bearer topsecret"} {
		if strings.Contains(got, secret) {
			t.Fatalf("SafePayload leaked %q: %s", secret, got)
		}
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("SafePayload did not mark redaction: %s", got)
	}
	if !json.Valid([]byte(got)) {
		t.Fatalf("SafePayload produced invalid JSON: %s", got)
	}
}

func TestSafePayloadNeverLeaksBoundarySecret(t *testing.T) {
	secret := "oc_sk_" + strings.Repeat("Z", 80)
	raw := []byte(strings.Repeat("a", DefaultPayloadLimit-10) + secret + " " + strings.Repeat("b", 100))

	got := SafePayload(raw)
	if strings.Contains(got, "oc_sk") || strings.Contains(got, "ZZZ") {
		t.Fatalf("boundary secret leaked: %s", got)
	}
	if !strings.Contains(got, truncationMarker(raw)) {
		t.Fatalf("missing truncation marker: %s", got)
	}
}

func TestSafePayloadTruncationMarkerAndHash(t *testing.T) {
	raw := []byte(strings.Repeat("x", DefaultPayloadLimit+100))

	got := SafePayload(raw)
	prefix, marker, found := strings.Cut(got, "...[truncated ")
	if !found {
		t.Fatalf("missing truncation marker: %s", got)
	}
	if len(prefix) != DefaultPayloadLimit {
		t.Fatalf("retained %d bytes, want %d", len(prefix), DefaultPayloadLimit)
	}
	if want := truncationMarker(raw); got[len(prefix):] != want {
		t.Fatalf("marker = %q, want %q", got[len(prefix):], want)
	}
	if !strings.HasSuffix(marker, "]") {
		t.Fatalf("marker is not closed: %q", marker)
	}
}

func TestSafePayloadHashUsesOriginalBytes(t *testing.T) {
	raw := []byte(strings.Repeat("secret ", DefaultPayloadLimit))
	got := SafePayload(raw)

	sum := sha256.Sum256(raw)
	want := fmt.Sprintf("...[truncated sha256=%x]", sum)
	if !strings.HasSuffix(got, want) {
		t.Fatalf("marker = %q, want suffix %q", got[DefaultPayloadLimit:], want)
	}
	if strings.HasSuffix(got, fmt.Sprintf("...[truncated sha256=%x]", sha256.Sum256([]byte(strings.Repeat("secret ", DefaultPayloadLimit-1))))) {
		t.Fatal("hash was not computed over the original raw bytes")
	}
}

func TestSafePayloadUnderLimitUnchangedAndIdempotent(t *testing.T) {
	raw := []byte(`{"authorization":"Bearer hidden","memo":"hello"}`)
	got := SafePayload(raw)
	if !json.Valid([]byte(got)) {
		t.Fatalf("invalid JSON: %s", got)
	}
	if got != llm.Redact(string(raw)) {
		t.Fatalf("SafePayload = %q, want plain redaction", got)
	}
	if again := llm.Redact(got); again != got {
		t.Fatalf("redaction is not idempotent: %q then %q", got, again)
	}
}

func TestSafePayloadUTF8Boundary(t *testing.T) {
	raw := []byte(strings.Repeat("a", DefaultPayloadLimit-1) + "é" + strings.Repeat("b", 100))
	got := SafePayload(raw)

	prefix, _, found := strings.Cut(got, "...[truncated ")
	if !found {
		t.Fatalf("missing truncation marker: %s", got)
	}
	if !utf8.ValidString(prefix) {
		t.Fatalf("truncated prefix is not valid UTF-8: %q", prefix)
	}
	if strings.ContainsRune(prefix, '\uFFFD') {
		t.Fatalf("truncated prefix contains a replacement rune: %q", prefix)
	}
	if len(prefix) != DefaultPayloadLimit-1 {
		t.Fatalf("retained %d bytes, want %d after backing off the split rune", len(prefix), DefaultPayloadLimit-1)
	}
}

// TestSafePayloadInvalidUTF8BeforeBoundary guards the truncation invariant
// when the payload itself contains an invalid byte before an otherwise valid
// cut boundary. Backing off only the final rune is not enough.
func TestSafePayloadInvalidUTF8BeforeBoundary(t *testing.T) {
	raw := []byte("a\xffb" + strings.Repeat("c", DefaultPayloadLimit+64))
	got := SafePayload(raw)
	prefix, _, found := strings.Cut(got, "...[truncated ")
	if !found {
		t.Fatalf("missing truncation marker: %s", got)
	}
	if !utf8.ValidString(prefix) {
		t.Fatalf("truncated prefix is not valid UTF-8: %q", prefix)
	}
	if strings.ContainsRune(prefix, '\uFFFD') {
		t.Fatalf("truncated prefix contains a replacement rune: %q", prefix)
	}
	if !strings.HasPrefix(prefix, "ab") {
		t.Fatalf("truncated prefix = %q, want the surrounding valid bytes retained", prefix)
	}
}

func TestSafePayloadDeterministic(t *testing.T) {
	raw := []byte(strings.Repeat("same ", DefaultPayloadLimit) + "oc_sk_key")
	if got, want := SafePayload(raw), SafePayload(raw); got != want {
		t.Fatal("SafePayload is not deterministic")
	}
}

func TestSafePayloadAttributeCaptureOffByDefault(t *testing.T) {
	t.Setenv(EnvCapturePayloads, "")
	raw := []byte(`{"result":"ok","authorization":"Bearer secret"}`)

	attr, ok := SafePayloadAttribute(attribute.Key(AttrPayload), raw)
	if ok {
		t.Fatalf("capture-off returned attribute %v", attr)
	}
	if attr.Valid() {
		t.Fatalf("capture-off returned valid attribute %v", attr)
	}
}

func TestSafePayloadAttributeCaptureOn(t *testing.T) {
	t.Setenv(EnvCapturePayloads, "true")
	raw := []byte(`{"result":"ok","authorization":"Bearer secret"}`)

	attr, ok := SafePayloadAttribute(attribute.Key(AttrPayload), raw)
	if !ok {
		t.Fatal("capture-on did not return an attribute")
	}
	if string(attr.Key) != AttrPayload {
		t.Fatalf("attribute key = %q", attr.Key)
	}
	if strings.Contains(attr.Value.AsString(), "secret") {
		t.Fatalf("attribute leaked a secret: %s", attr.Value.AsString())
	}
}

func TestSafePayloadAttributeCaptureOffEvenWhenPayloadHasNoSecret(t *testing.T) {
	t.Setenv(EnvCapturePayloads, "false")
	if _, ok := SafePayloadAttribute(attribute.Key(AttrPayload), []byte("plain result")); ok {
		t.Fatal("capture-off returned a payload attribute")
	}
}

func truncationMarker(raw []byte) string {
	sum := sha256.Sum256(raw)
	return fmt.Sprintf("...[truncated sha256=%x]", sum)
}
