package tools

import (
	"errors"
	"fmt"
)

// Structured error codes returned to the model in the envelope.
const (
	CodeInvalidArguments = "invalid_arguments"
	CodeUnknownTool      = "unknown_tool"
	CodeNotFound         = "not_found"
	CodeToolError        = "tool_error"
	CodeInternalError    = "internal_error"
	// CodeInjectionFlagged means the run has untrusted-data injection flags,
	// so proposal tools are disabled; escalate remains allowed (M2-07).
	CodeInjectionFlagged = "injection_flagged"
)

// Sentinel errors. Handlers may wrap these with %w to select an error code;
// alternatively they may return a *CodedError with a safe message.
var (
	// ErrInvalidRunContext means the caller did not provide a usable run id,
	// case id and sequence number. No tool call is recorded in this case.
	ErrInvalidRunContext = errors.New("tools: invalid run context")
	// ErrRecordFailed means the tool_calls row could not be written. The
	// caller must treat the call as failed; it is never silently continued.
	ErrRecordFailed = errors.New("tools: record tool call failed")
	// ErrInvalidTool means Register was given a nil tool or an empty name.
	ErrInvalidTool = errors.New("tools: invalid tool")
	// ErrInvalidSchema means the tool's input schema is not a closed object
	// schema or does not compile.
	ErrInvalidSchema = errors.New("tools: invalid tool schema")
	// ErrDuplicateTool means a tool with the same name is already registered.
	ErrDuplicateTool = errors.New("tools: duplicate tool")
	// ErrUnknownTool is returned by Lookup-style helpers; the runner converts
	// an unknown name into a recorded unknown_tool envelope.
	ErrUnknownTool = errors.New("tools: unknown tool")
	// ErrInvalidRef means an evidence reference is malformed.
	ErrInvalidRef = errors.New("tools: invalid evidence reference")
	// ErrInvalidArguments marks argument validation failures before a handler
	// runs.
	ErrInvalidArguments = errors.New("tools: invalid arguments")
	// ErrNotFound and ErrTool are the sentinel forms of the not_found and
	// tool_error structured codes.
	ErrNotFound = errors.New("tools: not found")
	ErrTool     = errors.New("tools: tool error")
	ErrInternal = errors.New("tools: internal error")
)

// CodedError is a handler error with an explicit structured code and a message
// that is safe to show the model. The message must never contain raw database
// errors, tokens or stack traces; untyped errors are mapped to a generic
// internal_error instead.
type CodedError struct {
	Code    string
	Message string
	Err     error
}

// Error implements error.
func (e *CodedError) Error() string {
	switch {
	case e == nil:
		return ""
	case e.Message != "":
		return e.Message
	case e.Err != nil:
		return e.Err.Error()
	default:
		return e.Code
	}
}

// Unwrap exposes the wrapped cause for errors.Is/errors.As.
func (e *CodedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// NotFound returns a not_found CodedError with a safe message.
func NotFound(message string) error {
	return &CodedError{Code: CodeNotFound, Message: message}
}

// Failure returns a tool_error CodedError with a safe message.
func Failure(message string) error {
	return &CodedError{Code: CodeToolError, Message: message}
}

func validErrorCode(code string) bool {
	switch code {
	case CodeInvalidArguments, CodeUnknownTool, CodeNotFound, CodeToolError, CodeInternalError, CodeInjectionFlagged:
		return true
	default:
		return false
	}
}

func defaultMessage(code string) string {
	switch code {
	case CodeInvalidArguments:
		return "invalid arguments"
	case CodeUnknownTool:
		return "unknown tool"
	case CodeNotFound:
		return "not found"
	case CodeToolError:
		return "tool call failed"
	case CodeInternalError:
		return "internal error"
	case CodeInjectionFlagged:
		return "run has injection flags; only escalate is allowed"
	default:
		return "internal error"
	}
}

// errorPayload maps a handler error to a structured code and a message that is
// safe to return to the model. Untyped errors never leak their text.
func errorPayload(err error) (code, message string) {
	if err == nil {
		return "", ""
	}
	var coded *CodedError
	if errors.As(err, &coded) && validErrorCode(coded.Code) {
		message := coded.Message
		if message == "" {
			message = defaultMessage(coded.Code)
		}
		return coded.Code, message
	}
	switch {
	case errors.Is(err, ErrInvalidArguments):
		return CodeInvalidArguments, defaultMessage(CodeInvalidArguments)
	case errors.Is(err, ErrNotFound):
		return CodeNotFound, defaultMessage(CodeNotFound)
	case errors.Is(err, ErrTool):
		return CodeToolError, defaultMessage(CodeToolError)
	case errors.Is(err, ErrInternal):
		return CodeInternalError, defaultMessage(CodeInternalError)
	default:
		return CodeInternalError, defaultMessage(CodeInternalError)
	}
}

func wrapInvalidArguments(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidArguments, fmt.Sprintf(format, args...))
}
