package approval

import (
	"context"
	"fmt"

	"github.com/TamasGorgics/recon-agent/internal/policy"
	"github.com/TamasGorgics/recon-agent/internal/policy/verify"
)

// VerificationService is the narrow M4-05 verifier surface the production
// adapter calls. *verify.Service satisfies it.
type VerificationService interface {
	Verify(ctx context.Context, proposalID string) (verify.Result, error)
}

// VerifierAdapter converts the M4-05 verify.Result into the frozen
// policy.Verification hand-off shape. The wrapped service persists
// proposals.verification and pattern_verified; the adapter only translates.
type VerifierAdapter struct {
	Service VerificationService
}

var _ Verifier = VerifierAdapter{}

// Verify runs the deterministic verifier and converts its result. A verifier
// domain failure is a Result with Passed=false and a nil error, so it flows to
// policy as T1; infrastructure errors are returned unchanged.
func (a VerifierAdapter) Verify(ctx context.Context, proposalID string) (policy.Verification, error) {
	if a.Service == nil {
		return policy.Verification{}, fmt.Errorf("%w: nil verification service", ErrInvalidRequest)
	}
	result, err := a.Service.Verify(ctx, proposalID)
	if err != nil {
		return policy.Verification{}, err
	}
	verification, err := result.Verification()
	if err != nil {
		return policy.Verification{}, fmt.Errorf("approval: convert verification: %w", err)
	}
	return verification, nil
}
