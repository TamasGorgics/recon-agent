package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/ingest"
)

func TestIngestValidUploads(t *testing.T) {
	tests := []struct {
		name       string
		parts      []formPart
		wantFormat string
		wantSource string
	}{
		{
			name: "psp_a with default format",
			parts: []formPart{
				{name: "source", value: "psp_a"},
				{name: "file", filename: "psp.csv", value: "psp-bytes"},
			},
			wantFormat: "psp_a",
			wantSource: "psp_a",
		},
		{
			name: "bank_x explicit format",
			parts: []formPart{
				{name: "source", value: "bank_x"},
				{name: "file", filename: "bank.csv", value: "bank-bytes"},
				{name: "format", value: "bank_x"},
			},
			wantFormat: "bank_x",
			wantSource: "bank_x",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.ingest.result = ingest.Result{
				BatchID:  "batch-1",
				Status:   ingest.StatusIngested,
				Parsed:   3,
				Inserted: 3,
			}
			body, contentType := multipartBody(t, test.parts...)
			response := h.expectStatus(http.MethodPost, RouteIngest, body, contentType, testAdminKey, http.StatusOK)
			defer func() { _ = response.Body.Close() }()

			var result IngestResult
			if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if result.BatchID != "batch-1" || result.Status != "ingested" || result.Parsed != 3 || result.Inserted != 3 {
				t.Fatalf("result = %+v", result)
			}
			if h.ingest.calls != 1 {
				t.Fatalf("ingest calls = %d, want 1", h.ingest.calls)
			}
			if h.ingest.got.Source != test.wantSource {
				t.Fatalf("source = %q, want %q", h.ingest.got.Source, test.wantSource)
			}
			if !strings.Contains(stringOfParser(h.ingest.got.Parser), expectedParserType(test.wantFormat)) {
				t.Fatalf("parser = %s, want %s", stringOfParser(h.ingest.got.Parser), expectedParserType(test.wantFormat))
			}
			if string(h.ingest.got.Data) != test.parts[1].value {
				t.Fatalf("file bytes = %q, want %q", h.ingest.got.Data, test.parts[1].value)
			}
		})
	}
}

func TestIngestQuarantineAndNoOp(t *testing.T) {
	t.Run("quarantined is 200 with reason", func(t *testing.T) {
		h := newHarness(t, nil)
		h.ingest.result = ingest.Result{
			BatchID:          "batch-q",
			Status:           ingest.StatusQuarantined,
			QuarantineReason: "control total mismatch",
		}
		body, contentType := multipartBody(t, formPart{name: "source", value: "psp_a"}, formPart{name: "file", value: "data"})
		response := h.expectStatus(http.MethodPost, RouteIngest, body, contentType, testAdminKey, http.StatusOK)
		defer func() { _ = response.Body.Close() }()
		var result IngestResult
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if result.Status != "quarantined" || result.QuarantineReason != "control total mismatch" {
			t.Fatalf("result = %+v", result)
		}
	})

	t.Run("no-op returns existing batch id", func(t *testing.T) {
		h := newHarness(t, nil)
		h.ingest.result = ingest.Result{BatchID: "existing-batch", Status: ingest.StatusIngested, NoOp: true}
		body, contentType := multipartBody(t, formPart{name: "source", value: "psp_a"}, formPart{name: "file", value: "data"})
		response := h.expectStatus(http.MethodPost, RouteIngest, body, contentType, testAdminKey, http.StatusOK)
		defer func() { _ = response.Body.Close() }()
		var result IngestResult
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !result.NoOp || result.BatchID != "existing-batch" {
			t.Fatalf("result = %+v", result)
		}
	})
}

func TestIngestRequestFailures(t *testing.T) {
	valid, _ := multipartBody(t, formPart{name: "source", value: "psp_a"}, formPart{name: "file", value: "data"})
	duplicateSource := rawMultipartBody("boundary123", [][2]string{{"source", "psp_a"}, {"source", "bank_x"}, {"file", "data"}})
	duplicateFile := rawMultipartBody("boundary123", [][2]string{{"source", "psp_a"}, {"file", "one"}, {"file", "two"}})
	unknownPart := rawMultipartBody("boundary123", [][2]string{{"source", "psp_a"}, {"file", "data"}, {"memo", "hello"}})

	tests := []struct {
		name        string
		body        string
		contentType string
		wantStatus  int
		wantCode    string
	}{
		{"missing source", rawMultipartBody("boundary123", [][2]string{{"file", "data"}}), "multipart/form-data; boundary=boundary123", http.StatusBadRequest, codeInvalidRequest},
		{"missing file", rawMultipartBody("boundary123", [][2]string{{"source", "psp_a"}}), "multipart/form-data; boundary=boundary123", http.StatusBadRequest, codeInvalidRequest},
		{"empty source", rawMultipartBody("boundary123", [][2]string{{"source", "  "}, {"file", "data"}}), "multipart/form-data; boundary=boundary123", http.StatusBadRequest, codeInvalidRequest},
		{"empty file", rawMultipartBody("boundary123", [][2]string{{"source", "psp_a"}, {"file", ""}}), "multipart/form-data; boundary=boundary123", http.StatusBadRequest, codeInvalidRequest},
		{"duplicate source", duplicateSource, "multipart/form-data; boundary=boundary123", http.StatusBadRequest, codeInvalidRequest},
		{"duplicate file", duplicateFile, "multipart/form-data; boundary=boundary123", http.StatusBadRequest, codeInvalidRequest},
		{"unknown part", unknownPart, "multipart/form-data; boundary=boundary123", http.StatusBadRequest, codeInvalidRequest},
		{"wrong content type", valid, "application/json", http.StatusUnsupportedMediaType, codeUnsupportedMedia},
		{"lookalike media type", valid, "multipart/form-datax; boundary=boundary123", http.StatusUnsupportedMediaType, codeUnsupportedMedia},
		{"missing boundary", valid, "multipart/form-data", http.StatusBadRequest, codeInvalidRequest},
		{"malformed boundary", valid, "multipart/form-data; boundary=", http.StatusBadRequest, codeInvalidRequest},
		{"boundary without value", valid, "multipart/form-data; boundary", http.StatusBadRequest, codeInvalidRequest},
		{"unknown source", rawMultipartBody("boundary123", [][2]string{{"source", "other"}, {"file", "data"}}), "multipart/form-data; boundary=boundary123", http.StatusBadRequest, codeUnsupportedSource},
		{"unknown format", rawMultipartBody("boundary123", [][2]string{{"source", "psp_a"}, {"format", "csv"}, {"file", "data"}}), "multipart/form-data; boundary=boundary123", http.StatusBadRequest, codeUnsupportedSource},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t, nil)
			response := h.expectStatus(http.MethodPost, RouteIngest, test.body, test.contentType, testAdminKey, test.wantStatus)
			defer func() { _ = response.Body.Close() }()
			assertErrorCode(t, response, test.wantCode)
			if h.ingest.calls != 0 {
				t.Fatal("ingest service was called for an invalid request")
			}
		})
	}
}

func TestIngestOversizedBody(t *testing.T) {
	h := newHarness(t, func(opts *Options) {
		opts.Config.MaxUploadBytes = 128
		opts.Config.MaxJSONBytes = 128
	})
	body, contentType := multipartBody(t,
		formPart{name: "source", value: "psp_a"},
		formPart{name: "file", value: strings.Repeat("x", 512)},
	)
	response := h.expectStatus(http.MethodPost, RouteIngest, body, contentType, testAdminKey, http.StatusRequestEntityTooLarge)
	defer func() { _ = response.Body.Close() }()
	assertErrorCode(t, response, codePayloadTooLarge)
	if h.ingest.calls != 0 {
		t.Fatal("ingest service was called for an oversized request")
	}
}

func TestIngestServiceErrors(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"parse error", ingest.ErrParse, http.StatusBadRequest, codeInvalidRequest},
		{"unknown source", ingest.ErrUnknownSource, http.StatusBadRequest, codeUnsupportedSource},
		{"internal", errInternalForTest{}, http.StatusInternalServerError, codeInternal},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.ingest.err = test.err
			body, contentType := multipartBody(t, formPart{name: "source", value: "psp_a"}, formPart{name: "file", value: "data"})
			response := h.expectStatus(http.MethodPost, RouteIngest, body, contentType, testAdminKey, test.wantStatus)
			defer func() { _ = response.Body.Close() }()
			assertErrorCode(t, response, test.wantCode)
			if test.wantStatus == http.StatusInternalServerError && strings.Contains(bodyOf(t, response), test.err.Error()) {
				t.Fatal("internal error leaked into the response")
			}
		})
	}
}

func stringOfParser(parser ingest.Parser) string {
	if parser == nil {
		return "<nil>"
	}
	switch parser.(type) {
	case *ingest.PSPParser:
		return "*ingest.PSPParser"
	case *ingest.BankParser:
		return "*ingest.BankParser"
	default:
		return "<unknown>"
	}
}

func expectedParserType(format string) string {
	switch format {
	case "psp_a":
		return "PSPParser"
	case "bank_x":
		return "BankParser"
	default:
		return ""
	}
}

type errInternalForTest struct{}

func (errInternalForTest) Error() string { return "db exploded: secret-detail" }
