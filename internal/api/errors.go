package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/TamasGorgics/recon-agent/internal/cases"
	"github.com/TamasGorgics/recon-agent/internal/ingest"
	"github.com/TamasGorgics/recon-agent/internal/match"
)

// Stable error codes from spec section 8.1.
const (
	codeInvalidJSON         = "invalid_json"
	codeInvalidRequest      = "invalid_request"
	codeInvalidQuery        = "invalid_query"
	codeUnsupportedFilter   = "unsupported_filter"
	codeUnsupportedSource   = "unsupported_source"
	codeInvalidScopeID      = "invalid_scope_id"
	codeInvalidDateRange    = "invalid_date_range"
	codeUnauthenticated     = "unauthenticated"
	codeForbidden           = "forbidden"
	codeNotFound            = "not_found"
	codeMethodNotAllowed    = "method_not_allowed"
	codeConflict            = "conflict"
	codeReplay              = "replay"
	codePayloadTooLarge     = "payload_too_large"
	codeUnsupportedMedia    = "unsupported_media_type"
	codeInternal            = "internal_error"
	messageUnauthenticated  = "authentication is required"
	messageForbidden        = "the API key role does not permit this action"
	messageNotFound         = "the requested resource was not found"
	messageMethodNotAllowed = "the method is not allowed for this resource"
	messageInternal         = "the server could not complete the request"
)

// errorEnvelope is the single public error shape.
type errorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	RequestID string `json:"request_id"`
}

// clientError is a handler-side failure that is safe to map onto the public
// envelope.
type clientError struct {
	status  int
	code    string
	message string
}

func newClientError(status int, code, message string) *clientError {
	return &clientError{status: status, code: code, message: message}
}

func (e *clientError) Error() string { return e.code + ": " + e.message }

// writeError writes the standard envelope. The request ID comes from the
// request context, or from the response header when the context is absent (for
// example a webhook fallback).
func (a *API) writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	requestID := RequestIDFromContext(r.Context())
	if requestID == "" {
		requestID = w.Header().Get("X-Request-ID")
	}
	envelope := errorEnvelope{RequestID: requestID}
	envelope.Error.Code = code
	envelope.Error.Message = message
	writeJSON(w, status, envelope)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// writeClientError writes a decoded handler failure.
func (a *API) writeClientError(w http.ResponseWriter, r *http.Request, err *clientError) {
	a.writeError(w, r, err.status, err.code, err.message)
}

// writeServiceError maps a service/DB failure onto the public envelope and logs
// the cause. The response never contains the service error text.
func (a *API) writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, message := mapServiceError(err)
	if status >= http.StatusInternalServerError {
		a.logError(r, "request failed", err)
	}
	a.writeError(w, r, status, code, message)
}

func mapServiceError(err error) (int, string, string) {
	switch {
	case errors.Is(err, cases.ErrCaseNotFound), errors.Is(err, match.ErrScopeNotFound):
		return http.StatusNotFound, codeNotFound, messageNotFound
	case errors.Is(err, match.ErrInvalidWindow), errors.Is(err, match.ErrInvalidRequest):
		return http.StatusBadRequest, codeInvalidRequest, "the request is invalid"
	case errors.Is(err, ingest.ErrUnknownSource):
		return http.StatusBadRequest, codeUnsupportedSource, "the source or format is not supported"
	case errors.Is(err, ingest.ErrInvalidRequest),
		errors.Is(err, ingest.ErrParse),
		errors.Is(err, ingest.ErrInvalidRecord):
		return http.StatusBadRequest, codeInvalidRequest, "the uploaded file is invalid"
	case errors.Is(err, cases.ErrInvalidRequest),
		errors.Is(err, cases.ErrNoBreaks),
		errors.Is(err, cases.ErrBreakNotFound),
		errors.Is(err, cases.ErrBreakInCase),
		errors.Is(err, cases.ErrBreakNotOpen):
		return http.StatusBadRequest, codeInvalidRequest, "the request is invalid"
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "22P02" {
		return http.StatusBadRequest, codeInvalidRequest, "the request is invalid"
	}
	return http.StatusInternalServerError, codeInternal, messageInternal
}

// logError records a server-side cause with correlation IDs. It never logs
// request headers, query strings, or bodies.
func (a *API) logError(r *http.Request, message string, err error) {
	attrs := []any{
		"request_id", RequestIDFromContext(r.Context()),
		"trace_id", traceIDFromContext(r.Context()),
		"error", err.Error(),
	}
	a.logger.Error(message, attrs...)
}
