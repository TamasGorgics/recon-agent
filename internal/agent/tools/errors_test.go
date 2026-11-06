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

// TestProposalErrorCodes proves every M2-08 proposal code survives
// validErrorCode and carries a non-generic safe default message, so the model
// sees the structured code instead of internal_error.
func TestProposalErrorCodes(t *testing.T) {
	t.Parallel()

	codes := []string{
		CodeCaseMismatch,
		CodeCaseNotOpen,
		CodeEvidenceNotRetrieved,
		CodeBusinessRuleViolation,
		CodeProposalAlreadyExists,
		CodeActiveProposalExists,
		CodeInvalidEvidence,
	}
	for _, code := range codes {
		code := code
		t.Run(code, func(t *testing.T) {
			t.Parallel()
			if !validErrorCode(code) {
				t.Fatalf("validErrorCode(%q) = false", code)
			}
			gotCode, message := errorPayload(&CodedError{Code: code})
			if gotCode != code {
				t.Fatalf("errorPayload code = %q, want %q", gotCode, code)
			}
			if message == "" || message != defaultMessage(code) {
				t.Fatalf("errorPayload message = %q, want %q", message, defaultMessage(code))
			}
			if message == defaultMessage(CodeInternalError) {
				t.Fatalf("%q fell back to the internal error message", code)
			}
		})
	}
}
