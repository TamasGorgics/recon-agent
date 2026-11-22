package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// phoenixProject is the project Phoenix creates by default. The demo uses it
// because no project management is needed for a local trace check.
const phoenixProject = "default"

// phoenixTrace is the subset of Phoenix's GET /v1/projects/{project}/traces
// response needed to prove receipt. Field names come from the pinned
// 22.0.1 OpenAPI schema (TraceData/TraceSpanData).
type phoenixTrace struct {
	TraceID string             `json:"trace_id"`
	Spans   []phoenixTraceSpan `json:"spans"`
}

type phoenixTraceSpan struct {
	SpanID     string  `json:"span_id"`
	ParentID   *string `json:"parent_id"`
	Name       string  `json:"name"`
	StatusCode string  `json:"status_code"`
}

type phoenixTracesResponse struct {
	Data []phoenixTrace `json:"data"`
}

// fetchPhoenixTrace requests recent traces with spans and returns the one
// whose trace_id matches. found is false when Phoenix answered successfully
// but has not received the trace yet.
func fetchPhoenixTrace(ctx context.Context, client *http.Client, baseURL, traceID string) (trace phoenixTrace, found bool, err error) {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	endpoint, err := url.Parse(strings.TrimRight(baseURL, "/") + "/v1/projects/" + phoenixProject + "/traces")
	if err != nil {
		return phoenixTrace{}, false, fmt.Errorf("parse phoenix URL: %w", err)
	}
	query := endpoint.Query()
	query.Set("limit", "100")
	query.Set("include_spans", "true")
	endpoint.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return phoenixTrace{}, false, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return phoenixTrace{}, false, fmt.Errorf("query phoenix: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return phoenixTrace{}, false, fmt.Errorf("read phoenix response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return phoenixTrace{}, false, fmt.Errorf("phoenix traces: status %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var payload phoenixTracesResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return phoenixTrace{}, false, fmt.Errorf("decode phoenix response: %w", err)
	}
	for _, candidate := range payload.Data {
		if candidate.TraceID == traceID {
			return candidate, true, nil
		}
	}
	return phoenixTrace{}, false, nil
}

// waitPhoenixTrace polls fetchPhoenixTrace until the trace appears or the
// timeout expires. The poll keeps the live check opt-in and robust against
// Phoenix's asynchronous ingestion.
func waitPhoenixTrace(ctx context.Context, client *http.Client, baseURL, traceID string, timeout time.Duration) (phoenixTrace, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		trace, found, err := fetchPhoenixTrace(ctx, client, baseURL, traceID)
		if err == nil && found {
			return trace, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			if lastErr != nil {
				return phoenixTrace{}, fmt.Errorf("phoenix receipt for trace %s: %w", traceID, lastErr)
			}
			return phoenixTrace{}, fmt.Errorf("phoenix receipt for trace %s: not returned by %s within %s", traceID, baseURL, timeout)
		}
		select {
		case <-ctx.Done():
			return phoenixTrace{}, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}
