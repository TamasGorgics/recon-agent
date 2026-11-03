package config

// LedgerConfig holds the account policy used by the ledger service (FIN-13).
//
// AdjustmentAccounts lists the account codes an adjustment transaction may
// target. FIN-13 names fees expense, FX gain/loss, rounding, and suspense
// accounts; operators may add the clearing or receivable accounts their
// correction flows touch. An empty list rejects every adjustment, which is the
// safe default when the chart of accounts is unknown.
//
// CustomerFundsAccounts lists account codes that require tier T2 approval. The
// ledger service returns ErrCustomerFundsT2 for any transaction touching one;
// the policy engine (M4) is responsible for the approval decision.
type LedgerConfig struct {
	AdjustmentAccounts    []string `yaml:"adjustment_accounts"`
	CustomerFundsAccounts []string `yaml:"customer_funds_accounts"`
}

// DefaultAdjustmentAccounts is the account-code allowlist used when the YAML
// configuration does not override ledger.adjustment_accounts. The names match
// the account classes FIN-13 names.
var DefaultAdjustmentAccounts = []string{
	"fees_expense",
	"fx_gain_loss",
	"rounding",
	"suspense",
}

// DefaultLedgerConfig returns the ledger policy defaults. It returns a fresh
// slice so callers cannot mutate the package default.
func DefaultLedgerConfig() LedgerConfig {
	return LedgerConfig{
		AdjustmentAccounts: append([]string(nil), DefaultAdjustmentAccounts...),
	}
}

// AllowsAdjustment reports whether an adjustment transaction may target the
// account with this code.
func (c LedgerConfig) AllowsAdjustment(code string) bool {
	return containsCode(c.AdjustmentAccounts, code)
}

// IsCustomerFunds reports whether the account with this code holds customer
// funds and therefore requires tier T2 approval.
func (c LedgerConfig) IsCustomerFunds(code string) bool {
	return containsCode(c.CustomerFundsAccounts, code)
}

func containsCode(codes []string, code string) bool {
	for _, candidate := range codes {
		if candidate == code {
			return true
		}
	}
	return false
}
