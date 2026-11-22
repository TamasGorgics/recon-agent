package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestFetchPhoenixTrace proves the level-2 receipt query path without a live
// Phoenix: exact REST path/query, trace lookup by ID, and span extraction.
func TestFetchPhoenixTrace(t *testing.T) {
	const traceID = "0123456789abcdef0123456789abcdef"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/v1/projects/default/traces" {
			t.Errorf("path = %q, want /v1/projects/default/traces", r.URL.Path)
		}
		if got := r.URL.Query().Get("limit"); got != "100" {
			t.Errorf("limit = %q, want 100", got)
		}
		if got := r.URL.Query().Get("include_spans"); got != "true" {
			t.Errorf("include_spans = %q, want true", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [
				{"trace_id": "ffffffffffffffffffffffffffffffff", "spans": []},
				{"trace_id": "` + traceID + `", "spans": [
					{"span_id": "1111111111111111", "parent_id": null, "name": "case.run", "status_code": "OK"},
					{"span_id": "2222222222222222", "parent_id": "1111111111111111", "name": "llm.call", "status_code": "OK"}
				]}
			],
			"next_cursor": null
		}`))
	}))
	defer server.Close()

	trace, found, err := fetchPhoenixTrace(context.Background(), server.Client(), server.URL, traceID)
	if err != nil {
		t.Fatalf("fetchPhoenixTrace: %v", err)
	}
	if !found {
		t.Fatal("trace was not found")
	}
	if trace.TraceID != traceID {
		t.Fatalf("trace_id = %q, want %q", trace.TraceID, traceID)
	}
	if len(trace.Spans) != 2 || trace.Spans[0].Name != "case.run" || trace.Spans[1].ParentID == nil {
		t.Fatalf("spans = %+v, want case.run plus a child", trace.Spans)
	}
}

func TestFetchPhoenixTraceRejectsNonOK(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "phoenix unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	_, _, err := fetchPhoenixTrace(context.Background(), server.Client(), server.URL, "0123456789abcdef0123456789abcdef")
	if err == nil {
		t.Fatal("non-200 response was accepted")
	}
}

func TestWaitPhoenixTraceHonorsContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data": [], "next_cursor": null}`))
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := waitPhoenixTrace(ctx, server.Client(), server.URL, "0123456789abcdef0123456789abcdef", time.Minute)
	if err == nil {
		t.Fatal("waitPhoenixTrace returned success with no matching trace")
	}
}
