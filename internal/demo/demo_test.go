package demo_test

import (
	"context"
	"errors"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/demo"
)

// TestResetRefusedInProduction pins the dev-only guard: --reset must never
// touch a production database, and the guard runs before any connection.
func TestResetRefusedInProduction(t *testing.T) {
	t.Setenv("RECON_ENV", "production")
	err := demo.Reset(context.Background(), "")
	if !errors.Is(err, demo.ErrResetProduction) {
		t.Fatalf("Reset error = %v, want ErrResetProduction", err)
	}
}

func TestResetRefusedInPRODUCTIONCaseInsensitively(t *testing.T) {
	t.Setenv("RECON_ENV", "PRODUCTION")
	err := demo.Reset(context.Background(), "")
	if !errors.Is(err, demo.ErrResetProduction) {
		t.Fatalf("Reset error = %v, want ErrResetProduction", err)
	}
}
