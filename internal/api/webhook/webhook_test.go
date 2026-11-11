package webhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/clock"
)

const (
	testSecret    = "super-secret-webhook-value"
	testSignature = "X-Test-Signature"
	testTimestamp = "X-Test-Timestamp"
)

func testOptions() Options {
	return Options{
		Secret:          []byte(testSecret),
		SignatureHeader: testSignature,
		TimestampHeader: testTimestamp,
	}
}

func sign(secret, prefix, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strings.TrimSuffix(prefix, "=")))
	mac.Write([]byte(":"))
	mac.Write([]byte(timestamp))
	mac.Write([]byte(":"))
	mac.Write(body)
	return prefix + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifierAcceptsValidAndRestoresBody(t *testing.T) {
	base := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	clk := clock.NewFake(base)
	body := []byte(`{"event":"payment.settled"}`)
	timestamp := strconv.FormatInt(base.Unix(), 10)

	var calls int
	var observed []byte
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var err error
		observed, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read restored body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	})

	verifier, err := New(Options{
		Secret:          []byte(testSecret),
		SignatureHeader: testSignature,
		TimestampHeader: testTimestamp,
		Clock:           clk,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/hook", bytes.NewReader(body))
	request.Header.Set(testSignature, sign(testSecret, DefaultPrefix, timestamp, body))
	request.Header.Set(testTimestamp, timestamp)
	request.Header.Set("X-Request-ID", "req-1")
	recorder := httptest.NewRecorder()

	verifier.Middleware(handler).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body)
	}
	if calls != 1 {
		t.Fatalf("handler calls = %d, want 1", calls)
	}
	if !bytes.Equal(observed, body) {
		t.Fatalf("restored body = %q, want %q", observed, body)
	}
}

func TestVerifierRejectsMutatedBodyAndSignature(t *testing.T) {
	base := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	timestamp := strconv.FormatInt(base.Unix(), 10)
	body := []byte(`{"event":"payment.settled"}`)
	signature := sign(testSecret, DefaultPrefix, timestamp, body)

	tests := []struct {
		name      string
		body      []byte
		signature string
	}{
		{name: "mutated body", body: []byte(`{"event":"payment.reversed"}`), signature: signature},
		{name: "mutated signature", body: body, signature: sign(testSecret, DefaultPrefix, timestamp, []byte("other"))},
		{name: "wrong prefix", body: body, signature: "v1=" + signature[len(DefaultPrefix):]},
		{name: "short signature", body: body, signature: DefaultPrefix + "abcd"},
		{name: "non-hex signature", body: body, signature: DefaultPrefix + strings.Repeat("z", sha256.Size*2)},
		{name: "missing signature", body: body, signature: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ })
			verifier, err := New(Options{
				Secret:          []byte(testSecret),
				SignatureHeader: testSignature,
				TimestampHeader: testTimestamp,
				Clock:           clock.NewFake(base),
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			request := httptest.NewRequest(http.MethodPost, "/hook", bytes.NewReader(test.body))
			if test.signature != "" {
				request.Header.Set(testSignature, test.signature)
			}
			request.Header.Set(testTimestamp, timestamp)
			recorder := httptest.NewRecorder()
			verifier.Middleware(handler).ServeHTTP(recorder, request)
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", recorder.Code)
			}
			if calls != 0 {
				t.Fatalf("handler calls = %d, want 0", calls)
			}
			assertEnvelope(t, recorder.Body.Bytes(), CodeUnauthenticated)
		})
	}
}

func TestVerifierRejectsStaleAndFutureTimestamps(t *testing.T) {
	base := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	body := []byte(`{}`)
	tests := []struct {
		name string
		ts   time.Time
	}{
		{name: "stale", ts: base.Add(-5*time.Minute - time.Second)},
		{name: "future", ts: base.Add(5*time.Minute + time.Second)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			timestamp := strconv.FormatInt(test.ts.Unix(), 10)
			calls := 0
			handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ })
			verifier, err := New(Options{
				Secret:          []byte(testSecret),
				SignatureHeader: testSignature,
				TimestampHeader: testTimestamp,
				Clock:           clock.NewFake(base),
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			request := httptest.NewRequest(http.MethodPost, "/hook", bytes.NewReader(body))
			request.Header.Set(testSignature, sign(testSecret, DefaultPrefix, timestamp, body))
			request.Header.Set(testTimestamp, timestamp)
			recorder := httptest.NewRecorder()
			verifier.Middleware(handler).ServeHTTP(recorder, request)
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", recorder.Code)
			}
			if calls != 0 {
				t.Fatalf("handler calls = %d, want 0", calls)
			}
		})
	}
}

func TestVerifierRejectsExactReplay(t *testing.T) {
	base := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	clk := clock.NewFake(base)
	body := []byte(`{"event":"payment.settled"}`)
	timestamp := strconv.FormatInt(base.Unix(), 10)
	signature := sign(testSecret, DefaultPrefix, timestamp, body)

	calls := 0
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ })
	verifier, err := New(Options{
		Secret:          []byte(testSecret),
		SignatureHeader: testSignature,
		TimestampHeader: testTimestamp,
		Clock:           clk,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	do := func() *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/hook", bytes.NewReader(body))
		request.Header.Set(testSignature, signature)
		request.Header.Set(testTimestamp, timestamp)
		recorder := httptest.NewRecorder()
		verifier.Middleware(handler).ServeHTTP(recorder, request)
		return recorder
	}
	if recorder := do(); recorder.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200", recorder.Code)
	}
	recorder := do()
	if recorder.Code != http.StatusConflict {
		t.Fatalf("replay status = %d, want 409; body=%s", recorder.Code, recorder.Body)
	}
	assertEnvelope(t, recorder.Body.Bytes(), CodeReplay)
	if calls != 1 {
		t.Fatalf("handler calls = %d, want 1", calls)
	}
}

func TestVerifierCanonicalizesSignatureHexForReplay(t *testing.T) {
	base := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	timestamp := strconv.FormatInt(base.Unix(), 10)
	body := []byte(`{"event":"payment.settled"}`)
	lower := sign(testSecret, DefaultPrefix, timestamp, body)
	hexPart := lower[len(DefaultPrefix):]
	upper := DefaultPrefix + strings.ToUpper(hexPart)
	mixed := DefaultPrefix + strings.ToUpper(hexPart[:16]) + strings.ToLower(hexPart[16:])

	tests := []struct {
		name   string
		first  string
		replay string
	}{
		{name: "lowercase then uppercase", first: lower, replay: upper},
		{name: "uppercase then lowercase", first: upper, replay: lower},
		{name: "lowercase then mixed case", first: lower, replay: mixed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ })
			verifier, err := New(Options{
				Secret:          []byte(testSecret),
				SignatureHeader: testSignature,
				TimestampHeader: testTimestamp,
				Clock:           clock.NewFake(base),
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			do := func(signature string) *httptest.ResponseRecorder {
				request := httptest.NewRequest(http.MethodPost, "/hook", bytes.NewReader(body))
				request.Header.Set(testSignature, signature)
				request.Header.Set(testTimestamp, timestamp)
				recorder := httptest.NewRecorder()
				verifier.Middleware(handler).ServeHTTP(recorder, request)
				return recorder
			}
			if recorder := do(test.first); recorder.Code != http.StatusOK {
				t.Fatalf("first status = %d, want 200; body=%s", recorder.Code, recorder.Body)
			}
			recorder := do(test.replay)
			if recorder.Code != http.StatusConflict {
				t.Fatalf("case-variant replay status = %d, want 409; body=%s", recorder.Code, recorder.Body)
			}
			assertEnvelope(t, recorder.Body.Bytes(), CodeReplay)
			if calls != 1 {
				t.Fatalf("handler calls = %d, want 1", calls)
			}
		})
	}
}

func TestReplayKeyCanonicalizesSignatureBytes(t *testing.T) {
	base := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	timestamp := strconv.FormatInt(base.Unix(), 10)
	body := []byte(`{}`)
	lower := sign(testSecret, DefaultPrefix, timestamp, body)
	hexPart := lower[len(DefaultPrefix):]
	upper := DefaultPrefix + strings.ToUpper(hexPart)

	parsedLower, ok := parseSignature(lower, DefaultPrefix)
	if !ok {
		t.Fatal("parseSignature rejected the lowercase signature")
	}
	parsedUpper, ok := parseSignature(upper, DefaultPrefix)
	if !ok {
		t.Fatal("parseSignature rejected the uppercase signature")
	}
	lowerKey := replayKey(timestamp, parsedLower)
	if upperKey := replayKey(timestamp, parsedUpper); upperKey != lowerKey {
		t.Fatalf("case variants produced different replay keys: %q vs %q", lowerKey, upperKey)
	}
	if other := replayKey(timestamp+"0", parsedLower); other == lowerKey {
		t.Fatal("different timestamps produced the same replay key")
	}
	otherSignature := append([]byte(nil), parsedLower...)
	otherSignature[0] ^= 0xff
	if other := replayKey(timestamp, otherSignature); other == lowerKey {
		t.Fatal("different signatures produced the same replay key")
	}
}

func TestVerifierAcceptsRetryWithNewSignatureAfterTTL(t *testing.T) {
	base := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	clk := clock.NewFake(base)
	body := []byte(`{}`)

	calls := 0
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ })
	verifier, err := New(Options{
		Secret:          []byte(testSecret),
		SignatureHeader: testSignature,
		TimestampHeader: testTimestamp,
		Clock:           clk,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	first := base
	firstTS := strconv.FormatInt(first.Unix(), 10)
	request := httptest.NewRequest(http.MethodPost, "/hook", bytes.NewReader(body))
	request.Header.Set(testSignature, sign(testSecret, DefaultPrefix, firstTS, body))
	request.Header.Set(testTimestamp, firstTS)
	recorder := httptest.NewRecorder()
	verifier.Middleware(handler).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("first status = %d", recorder.Code)
	}

	// Advance past the replay TTL, then use a fresh timestamp and signature.
	clk.Advance(2*DefaultTolerance + time.Second)
	second := clk.Now()
	secondTS := strconv.FormatInt(second.Unix(), 10)
	request = httptest.NewRequest(http.MethodPost, "/hook", bytes.NewReader(body))
	request.Header.Set(testSignature, sign(testSecret, DefaultPrefix, secondTS, body))
	request.Header.Set(testTimestamp, secondTS)
	recorder = httptest.NewRecorder()
	verifier.Middleware(handler).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200; body=%s", recorder.Code, recorder.Body)
	}
	if calls != 2 {
		t.Fatalf("handler calls = %d, want 2", calls)
	}
}

func TestVerifierRejectsMisconfiguredHeadersAndOversize(t *testing.T) {
	base := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	body := []byte("body that is longer than four bytes")
	timestamp := strconv.FormatInt(base.Unix(), 10)
	tests := []struct {
		name      string
		configure func(*http.Request)
		want      int
	}{
		{name: "missing timestamp", configure: func(r *http.Request) {
			r.Header.Set(testSignature, sign(testSecret, DefaultPrefix, timestamp, body))
		}, want: http.StatusUnauthorized},
		{name: "garbage timestamp", configure: func(r *http.Request) {
			r.Header.Set(testTimestamp, "12x")
			r.Header.Set(testSignature, sign(testSecret, DefaultPrefix, "12x", body))
		}, want: http.StatusUnauthorized},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			verifier, err := New(Options{
				Secret:          []byte(testSecret),
				SignatureHeader: testSignature,
				TimestampHeader: testTimestamp,
				Clock:           clock.NewFake(base),
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			request := httptest.NewRequest(http.MethodPost, "/hook", bytes.NewReader(body))
			test.configure(request)
			recorder := httptest.NewRecorder()
			verifier.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(recorder, request)
			if recorder.Code != test.want {
				t.Fatalf("status = %d, want %d", recorder.Code, test.want)
			}
		})
	}

	verifier, err := New(Options{
		Secret:          []byte(testSecret),
		SignatureHeader: testSignature,
		TimestampHeader: testTimestamp,
		MaxBodyBytes:    4,
		Clock:           clock.NewFake(base),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/hook", bytes.NewReader(body))
	request.Header.Set(testTimestamp, timestamp)
	request.Header.Set(testSignature, sign(testSecret, DefaultPrefix, timestamp, body))
	recorder := httptest.NewRecorder()
	verifier.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize status = %d, want 413", recorder.Code)
	}
	assertEnvelope(t, recorder.Body.Bytes(), CodePayloadTooLarge)
}

func TestVerifierLogsNoSecretSignatureOrBody(t *testing.T) {
	base := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	body := []byte(`{"memo":"raw-secret-body-value"}`)
	timestamp := strconv.FormatInt(base.Unix(), 10)
	mutated := append(append([]byte(nil), body...), []byte("mutation")...)
	signature := sign(testSecret, DefaultPrefix, timestamp, mutated)

	var logs bytes.Buffer
	verifier, err := New(Options{
		Secret:          []byte(testSecret),
		SignatureHeader: testSignature,
		TimestampHeader: testTimestamp,
		Clock:           clock.NewFake(base),
		Logger:          slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/hook", bytes.NewReader(body))
	request.Header.Set(testSignature, signature)
	request.Header.Set(testTimestamp, timestamp)
	recorder := httptest.NewRecorder()
	verifier.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", recorder.Code)
	}
	logged := logs.String()
	for _, forbidden := range []string{testSecret, signature, "raw-secret-body-value"} {
		if strings.Contains(logged, forbidden) {
			t.Fatalf("log leaked %q: %s", forbidden, logged)
		}
	}
	if !strings.Contains(logged, "reason") {
		t.Fatalf("log without a reason: %s", logged)
	}
}

func TestVerifierAndOptionsFormattingRedactsSecret(t *testing.T) {
	opts := testOptions()
	verifier, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	secretBytes := fmt.Sprintf("%v", []byte(testSecret))
	secretDecimal := strings.ReplaceAll(secretBytes, " ", ", ")
	secretGo := fmt.Sprintf("%#v", []byte(testSecret))
	values := map[string]any{
		"verifier value":   *verifier,
		"verifier pointer": verifier,
		"options value":    opts,
		"options pointer":  &opts,
	}
	for name, value := range values {
		for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
			out := fmt.Sprintf(format, value)
			for _, secret := range []string{testSecret, secretBytes, secretDecimal, secretGo} {
				if strings.Contains(out, secret) {
					t.Fatalf("%s %s leaked the webhook secret: %s", name, format, out)
				}
			}
		}
	}
	for name, out := range map[string]string{
		"Verifier.GoString": verifier.GoString(),
		"Options.GoString":  opts.GoString(),
	} {
		for _, secret := range []string{testSecret, secretBytes, secretDecimal, secretGo} {
			if strings.Contains(out, secret) {
				t.Fatalf("%s leaked the webhook secret: %s", name, out)
			}
		}
	}
}

func TestVerifierOptionsValidation(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("New accepted an empty secret")
	}
	opts := testOptions()
	opts.SignatureHeader = ""
	if _, err := New(opts); err == nil {
		t.Fatal("New accepted an empty signature header")
	}
	opts = testOptions()
	opts.TimestampHeader = ""
	if _, err := New(opts); err == nil {
		t.Fatal("New accepted an empty timestamp header")
	}
}

func TestMemoryReplayCache(t *testing.T) {
	base := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	cache := NewMemoryReplayCache(2)
	if err := cache.CheckAndStore("a", base, time.Minute); err != nil {
		t.Fatalf("store a: %v", err)
	}
	if err := cache.CheckAndStore("a", base.Add(30*time.Second), time.Minute); !isErr(err, ErrReplay) {
		t.Fatalf("second a = %v, want ErrReplay", err)
	}
	if err := cache.CheckAndStore("b", base, time.Minute); err != nil {
		t.Fatalf("store b: %v", err)
	}
	if err := cache.CheckAndStore("c", base, time.Minute); !isErr(err, ErrReplayCacheFull) {
		t.Fatalf("full cache = %v, want ErrReplayCacheFull", err)
	}
	// After the TTL both entries are pruned lazily, so c fits.
	if err := cache.CheckAndStore("c", base.Add(2*time.Minute), time.Minute); err != nil {
		t.Fatalf("store c after expiry: %v", err)
	}
}

func isErr(got, want error) bool {
	return errors.Is(got, want)
}

func assertEnvelope(t *testing.T, data []byte, wantCode string) {
	t.Helper()
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("decode envelope %q: %v", data, err)
	}
	if envelope.Error.Code != wantCode {
		t.Fatalf("error code = %q, want %q", envelope.Error.Code, wantCode)
	}
	if envelope.Error.Message == "" {
		t.Fatal("error message is empty")
	}
}
