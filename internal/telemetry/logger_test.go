package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func decodeLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

func TestTraceIDRoundTrip(t *testing.T) {
	ctx := WithTraceID(context.Background(), "trace-123")
	if got, want := TraceID(ctx), "trace-123"; got != want {
		t.Fatalf("TraceID = %q, want %q", got, want)
	}
	if got := TraceID(context.Background()); got != "" {
		t.Fatalf("TraceID(background) = %q, want empty", got)
	}
}

func TestFromContextAddsTraceID(t *testing.T) {
	var buf bytes.Buffer
	base := NewLogger(&buf, slog.LevelInfo)
	ctx := WithTraceID(context.Background(), "trace-abc")

	FromContext(ctx, base).Info("handled case")

	records := decodeLines(t, &buf)
	if len(records) != 1 {
		t.Fatalf("got %d log records, want 1", len(records))
	}
	if got, want := records[0]["trace_id"], "trace-abc"; got != want {
		t.Fatalf("trace_id = %v, want %q", got, want)
	}
	if got, want := records[0]["msg"], "handled case"; got != want {
		t.Fatalf("msg = %v, want %q", got, want)
	}
}

func TestFromContextOmitsTraceID(t *testing.T) {
	var buf bytes.Buffer
	base := NewLogger(&buf, slog.LevelInfo)

	FromContext(context.Background(), base).Info("no trace")
	FromContext(WithTraceID(context.Background(), ""), base).Info("empty trace")

	records := decodeLines(t, &buf)
	if len(records) != 2 {
		t.Fatalf("got %d log records, want 2", len(records))
	}
	for i, record := range records {
		if _, ok := record["trace_id"]; ok {
			t.Fatalf("record %d has trace_id without a trace: %v", i, record)
		}
	}
}

func TestNewLoggerLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(&buf, slog.LevelInfo)

	logger.Debug("debug message")
	logger.Info("info message")
	logger.Warn("warn message")

	records := decodeLines(t, &buf)
	if len(records) != 2 {
		t.Fatalf("got %d log records, want 2", len(records))
	}
	if got, want := records[0]["msg"], "info message"; got != want {
		t.Fatalf("first msg = %v, want %q", got, want)
	}
	if got, want := records[1]["msg"], "warn message"; got != want {
		t.Fatalf("second msg = %v, want %q", got, want)
	}
}
