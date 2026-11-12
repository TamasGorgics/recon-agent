//go:build integration

// covers: FIN-13, FIN-34
package policy_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/policy"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

func loadedForTest(t *testing.T) policy.LoadedConfig {
	t.Helper()
	loaded, err := policy.LoadFile("../../policy.yaml")
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	return loaded
}

func engineForTest(t *testing.T) *policy.Engine {
	t.Helper()
	loaded := loadedForTest(t)
	engine, err := policy.NewEngine(loaded, policy.Options{CustomerFundsAccounts: []string{"customer_funds"}})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return engine
}

// ensurePolicyVersion mirrors the M4-11 obligation: PersistDecision only
// accepts a config hash that already has a policy_config_versions row.
func ensurePolicyVersion(t *testing.T, ctx context.Context, beginner db.Beginner, loaded policy.LoadedConfig) {
	t.Helper()
	if err := db.WithTx(ctx, beginner, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return loaded.EnsureVersion(ctx, tx, time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC))
	}); err != nil {
		t.Fatalf("EnsureVersion: %v", err)
	}
}

func decisionForTier(t *testing.T, engine *policy.Engine, tier policy.Tier) policy.Decision {
	t.Helper()
	verified := policy.PatternFeeVariance
	proposal := policy.Proposal{
		ID:              "case-1",
		CaseID:          "case-1",
		Type:            policy.TypeLink,
		Confidence:      decimal.RequireFromString("0.95"),
		Evidence:        []string{"evidence-1"},
		PatternVerified: &verified,
	}
	verification := policy.Verification{Pattern: policy.PatternFeeVariance, Passed: true}
	facts := policy.Facts{
		ValueMinor:       100,
		Currency:         "USD",
		CaseCurrency:     "USD",
		AccountCodes:     []string{"fees_expense"},
		EvidenceValid:    true,
		AutoApplyEnabled: true,
	}
	switch tier {
	case policy.TierT1:
		verification = policy.Verification{}
	case policy.TierT2:
		facts.ValueMinor = 50001
	case policy.TierT0:
	default:
		t.Fatalf("decisionForTier does not build %s", tier)
	}
	decision := engine.Evaluate(proposal, verification, facts)
	if decision.Tier != tier {
		t.Fatalf("built decision tier = %s, want %s", decision.Tier, tier)
	}
	return decision
}

func persist(t *testing.T, ctx context.Context, beginner db.Beginner, proposalID string, decision policy.Decision) error {
	t.Helper()
	return db.WithTx(ctx, beginner, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return policy.PersistDecision(ctx, tx, proposalID, decision)
	})
}

// TestPersistDecisionIntegration proves PersistDecision writes exactly
// risk_tier/policy_decision as recon_app and leaves every other column alone.
func TestPersistDecisionIntegration(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	app := openRolePool(t, d, db.RoleApp)
	engine := engineForTest(t)
	ensurePolicyVersion(t, ctx, app, loadedForTest(t))

	wantExpiry := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tier := range []policy.Tier{policy.TierT0, policy.TierT1, policy.TierT2} {
		tier := tier
		t.Run(string(tier), func(t *testing.T) {
			_, _, proposalID := seedProposal(t, d)
			decision := decisionForTier(t, engine, tier)

			if err := persist(t, ctx, app, proposalID, decision); err != nil {
				t.Fatalf("PersistDecision: %v", err)
			}

			stored := readProposal(t, d, proposalID)
			if stored.Tier == nil || *stored.Tier != string(decision.Tier) {
				t.Fatalf("risk_tier = %v, want %s", stored.Tier, decision.Tier)
			}
			var equal bool
			if err := d.Pool.QueryRow(ctx,
				`SELECT policy_decision = $2::jsonb FROM proposals WHERE id = $1::uuid`,
				proposalID, string(mustJSON(t, decision))).Scan(&equal); err != nil {
				t.Fatalf("compare decision JSON: %v", err)
			}
			if !equal {
				t.Fatalf("stored policy_decision does not equal %s", mustJSON(t, decision))
			}
			var persisted policy.Decision
			if err := json.Unmarshal(stored.Decision, &persisted); err != nil {
				t.Fatalf("decode persisted decision: %v", err)
			}
			if !reflect.DeepEqual(persisted, decision) {
				t.Fatalf("persisted decision = %+v, want %+v", persisted, decision)
			}
			if stored.Status != "proposed" || stored.Version != 0 {
				t.Fatalf("status/version = %s/%d, want proposed/0", stored.Status, stored.Version)
			}
			if stored.ExpiresAt == nil || !stored.ExpiresAt.Equal(wantExpiry) {
				t.Fatalf("expires_at = %v, want unchanged %s", stored.ExpiresAt, wantExpiry)
			}
			if stored.Rationale != "seed" || stored.StateHash != "state-hash" || stored.Confidence != "0.9000" {
				t.Fatalf("PersistDecision changed a non-decision column: %+v", stored)
			}
			if audits := countProposalAudits(t, d, proposalID); audits != 0 {
				t.Fatalf("PersistDecision wrote %d audit rows, want 0", audits)
			}

			// Repeating the same decision is a column-level idempotent write.
			if err := persist(t, ctx, app, proposalID, decision); err != nil {
				t.Fatalf("second PersistDecision: %v", err)
			}
			again := readProposal(t, d, proposalID)
			if again.Version != 0 || !reflect.DeepEqual(again.Decision, stored.Decision) {
				t.Fatalf("second PersistDecision changed version/decision: %+v", again)
			}
		})
	}
}

// TestPersistDecisionUnknownConfig proves a decision cannot name a policy
// document that was never versioned: ErrUnknownConfig before any proposal write.
func TestPersistDecisionUnknownConfig(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	app := openRolePool(t, d, db.RoleApp)
	engine := engineForTest(t)
	ensurePolicyVersion(t, ctx, app, loadedForTest(t))

	_, _, proposalID := seedProposal(t, d)
	decision := decisionForTier(t, engine, policy.TierT0)
	decision.ConfigHash = strings.Repeat("f", 64)

	err := persist(t, ctx, app, proposalID, decision)
	if !errors.Is(err, policy.ErrUnknownConfig) {
		t.Fatalf("PersistDecision error = %v, want ErrUnknownConfig", err)
	}
	stored := readProposal(t, d, proposalID)
	if stored.Tier != nil || stored.Decision != nil || stored.Version != 0 || stored.Status != "proposed" {
		t.Fatalf("unknown config wrote a row change: %+v", stored)
	}
	if audits := countProposalAudits(t, d, proposalID); audits != 0 {
		t.Fatalf("unknown config wrote %d audit rows, want 0", audits)
	}
}

func mustJSON(t *testing.T, decision policy.Decision) []byte {
	t.Helper()
	data, err := decision.JSON()
	if err != nil {
		t.Fatalf("Decision.JSON: %v", err)
	}
	return data
}

// TestPersistDecisionErrors proves the typed pre-SQL rejections and the
// not-found mapping.
func TestPersistDecisionErrors(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	app := openRolePool(t, d, db.RoleApp)
	engine := engineForTest(t)
	ensurePolicyVersion(t, ctx, app, loadedForTest(t))
	decision := decisionForTier(t, engine, policy.TierT0)

	_, _, proposalID := seedProposal(t, d)

	for _, tc := range []struct {
		name string
		id   string
		want error
	}{
		{"malformed", "not-a-uuid", policy.ErrInvalidProposalID},
		{"uppercase", strings.ToUpper("abcdef01-2345-4678-9abc-def012345678"), policy.ErrInvalidProposalID},
		{"braced", "{abcdef01-2345-4678-9abc-def012345678}", policy.ErrInvalidProposalID},
		{"unknown", "00000000-0000-4000-8000-000000000000", policy.ErrProposalNotFound},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := persist(t, ctx, app, tc.id, decision)
			if !errors.Is(err, tc.want) {
				t.Fatalf("PersistDecision(%q) error = %v, want %v", tc.id, err, tc.want)
			}
		})
	}

	invalid := policy.Decision{
		SchemaVersion: 1,
		Tier:          policy.TierT0,
		Rule:          policy.CodeAutoApply,
		Reasons:       []string{policy.CodeAutoApply},
		ConfigHash:    "short",
	}
	if err := persist(t, ctx, app, proposalID, invalid); !errors.Is(err, policy.ErrInvalidDecision) {
		t.Fatalf("invalid decision error = %v, want ErrInvalidDecision", err)
	}
	stored := readProposal(t, d, proposalID)
	if stored.Tier != nil || stored.Decision != nil || stored.Version != 0 || stored.Status != "proposed" {
		t.Fatalf("invalid decision wrote a row change: %+v", stored)
	}
}
