package api

import (
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/TamasGorgics/recon-agent/internal/ingest"
)

// maxSourceFieldBytes bounds the small "source"/"format" text parts. The file
// part is bounded by the request body cap.
const maxSourceFieldBytes = 1024

// handleIngest implements POST /v1/ingest/files. Authentication and role checks
// have already run; body parsing failures never reach the pipeline.
func (a *API) handleIngest(w http.ResponseWriter, r *http.Request) {
	if err := requireMultipartContentType(r); err != nil {
		a.writeClientError(w, r, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, a.cfg.MaxUploadBytes)
	reader, err := r.MultipartReader()
	if err != nil {
		a.writeClientError(w, r, newClientError(http.StatusBadRequest, codeInvalidRequest,
			"request body must be multipart/form-data"))
		return
	}

	var (
		source     string
		format     string
		fileName   string
		data       []byte
		haveSource bool
		haveFormat bool
		haveFile   bool
	)
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			a.writeClientError(w, r, multipartReadError(err))
			return
		}
		switch part.FormName() {
		case "source":
			if haveSource {
				_ = part.Close()
				a.writeClientError(w, r, duplicatePartError("source"))
				return
			}
			haveSource = true
			source, err = readTextPart(part)
			if err != nil {
				_ = part.Close()
				a.writeClientError(w, r, multipartReadError(err))
				return
			}
		case "format":
			if haveFormat {
				_ = part.Close()
				a.writeClientError(w, r, duplicatePartError("format"))
				return
			}
			haveFormat = true
			format, err = readTextPart(part)
			if err != nil {
				_ = part.Close()
				a.writeClientError(w, r, multipartReadError(err))
				return
			}
		case "file":
			if haveFile {
				_ = part.Close()
				a.writeClientError(w, r, duplicatePartError("file"))
				return
			}
			haveFile = true
			fileName = part.FileName()
			data, err = io.ReadAll(part)
			if err != nil {
				_ = part.Close()
				a.writeClientError(w, r, multipartReadError(err))
				return
			}
		default:
			_ = part.Close()
			a.writeClientError(w, r, newClientError(http.StatusBadRequest, codeInvalidRequest,
				"request contains an unsupported part"))
			return
		}
		if err := part.Close(); err != nil {
			a.writeClientError(w, r, multipartReadError(err))
			return
		}
	}

	source = strings.TrimSpace(source)
	if !haveSource || source == "" {
		a.writeClientError(w, r, newClientError(http.StatusBadRequest, codeInvalidRequest,
			"source is required"))
		return
	}
	if !haveFile {
		a.writeClientError(w, r, newClientError(http.StatusBadRequest, codeInvalidRequest,
			"file is required"))
		return
	}
	if len(data) == 0 {
		a.writeClientError(w, r, newClientError(http.StatusBadRequest, codeInvalidRequest,
			"file is empty"))
		return
	}

	parser, ok := parserFor(source)
	if !ok {
		a.writeError(w, r, http.StatusBadRequest, codeUnsupportedSource,
			"the source or format is not supported")
		return
	}
	if strings.TrimSpace(format) != "" {
		format = strings.TrimSpace(format)
		parser, ok = parserFor(format)
		if !ok {
			a.writeError(w, r, http.StatusBadRequest, codeUnsupportedSource,
				"the source or format is not supported")
			return
		}
	}

	result, err := a.ingest.Ingest(r.Context(), ingest.Request{
		Source:   source,
		FileName: fileName,
		Data:     data,
		Parser:   parser,
	})
	if err != nil {
		a.writeServiceError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, IngestResult{
		BatchID:          result.BatchID,
		Status:           string(result.Status),
		NoOp:             result.NoOp,
		Parsed:           result.Parsed,
		Inserted:         result.Inserted,
		Skipped:          result.Skipped,
		QuarantineReason: result.QuarantineReason,
	})
}

// parserFor is the fixed ingest parser registry. The API never accepts an
// arbitrary parser name.
func parserFor(source string) (ingest.Parser, bool) {
	switch source {
	case "psp_a":
		return ingest.NewPSPParser(), true
	case "bank_x":
		return ingest.NewBankParser(), true
	default:
		return nil, false
	}
}

func readTextPart(part *multipart.Part) (string, error) {
	data, err := io.ReadAll(io.LimitReader(part, maxSourceFieldBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxSourceFieldBytes {
		return "", errors.New("api: multipart text part is too large")
	}
	return string(data), nil
}

func duplicatePartError(name string) *clientError {
	return newClientError(http.StatusBadRequest, codeInvalidRequest,
		"multipart part "+name+" may appear only once")
}

// multipartReadError maps a body-cap overrun to 413 and every other read error
// to the generic 400 envelope.
func multipartReadError(err error) *clientError {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return newClientError(http.StatusRequestEntityTooLarge, codePayloadTooLarge,
			"request body is too large")
	}
	return newClientError(http.StatusBadRequest, codeInvalidRequest,
		"request body could not be read")
}
