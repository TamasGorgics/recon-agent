package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
)

var errDuplicateJSONKey = errors.New("api: duplicate JSON object key")

// requireJSONContentType accepts application/json with optional parameters.
func requireJSONContentType(r *http.Request) *clientError {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		return newClientError(http.StatusUnsupportedMediaType, codeUnsupportedMedia, "content type must be application/json")
	}
	return nil
}

// requireMultipartContentType accepts multipart/form-data with a boundary.
// A wrong media type is 415 unsupported_media_type. A multipart media type
// without a usable boundary is a malformed request body, not an unsupported
// media type, so it maps to 400 invalid_request (spec section 8.6).
func requireMultipartContentType(r *http.Request) *clientError {
	contentType := r.Header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		rawMediaType := contentType
		if i := strings.IndexByte(rawMediaType, ';'); i >= 0 {
			rawMediaType = rawMediaType[:i]
		}
		if strings.EqualFold(strings.TrimSpace(rawMediaType), "multipart/form-data") {
			return newClientError(http.StatusBadRequest, codeInvalidRequest, "multipart boundary is required")
		}
		return newClientError(http.StatusUnsupportedMediaType, codeUnsupportedMedia, "content type must be multipart/form-data with a boundary")
	}
	if !strings.EqualFold(mediaType, "multipart/form-data") {
		return newClientError(http.StatusUnsupportedMediaType, codeUnsupportedMedia, "content type must be multipart/form-data with a boundary")
	}
	if params["boundary"] == "" {
		return newClientError(http.StatusBadRequest, codeInvalidRequest, "multipart boundary is required")
	}
	return nil
}

// decodeJSONBody reads at most maxBytes, rejects malformed or duplicate-key
// JSON, and strict-decodes exactly one value into dst.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any, maxBytes int64) *clientError {
	if err := requireJSONContentType(r); err != nil {
		return err
	}
	limited := http.MaxBytesReader(w, r.Body, maxBytes)
	defer func() { _ = limited.Close() }()
	data, err := io.ReadAll(limited)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return newClientError(http.StatusRequestEntityTooLarge, codePayloadTooLarge, "request body is too large")
		}
		return newClientError(http.StatusBadRequest, codeInvalidJSON, "request body could not be read")
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return newClientError(http.StatusBadRequest, codeInvalidJSON, "request body is required")
	}
	if err := validateJSONDocument(data); err != nil {
		return newClientError(http.StatusBadRequest, codeInvalidJSON, "request body is not valid JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return newClientError(http.StatusBadRequest, codeInvalidJSON, "request body is not valid JSON")
	}
	return nil
}

// validateJSONDocument rejects duplicate object keys anywhere in data and
// requires exactly one top-level JSON value. encoding/json silently keeps the
// last duplicate key, so the structure is checked before the typed decode.
func validateJSONDocument(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := scanJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("api: trailing JSON value")
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("api: object key is not a string")
			}
			if _, exists := seen[key]; exists {
				return errDuplicateJSONKey
			}
			seen[key] = struct{}{}
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return nil
	}
}
