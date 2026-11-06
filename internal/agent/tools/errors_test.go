package tools

import "testing"

func TestInjectionFlaggedCode(t *testing.T) {
	t.Parallel()

	if !validErrorCode(CodeInjectionFlagged) {
		t.Fatalf("validErrorCode(%q) = false", CodeInjectionFlagged)
	}
	code, message := errorPayload(&CodedError{Code: CodeInjectionFlagged})
	if code != CodeInjectionFlagged {
		t.Fatalf("errorPayload code = %q, want %q", code, CodeInjectionFlagged)
	}
	if message != defaultMessage(CodeInjectionFlagged) || message == "" {
		t.Fatalf("errorPayload default message = %q", message)
	}
	if got := defaultMessage(CodeInjectionFlagged); got == "internal error" {
		t.Fatal("injection_flagged fell back to the internal error message")
	}
}
