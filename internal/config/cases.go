package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// DefaultMissingSettlementSLABusinessDays is the missing-settlement SLA when
// cases.missing_settlement_sla_business_days is omitted (spec section 6).
const DefaultMissingSettlementSLABusinessDays = 5

// MissingSettlementSLAKey is the YAML key and error name for the SLA day count.
const MissingSettlementSLAKey = "cases.missing_settlement_sla_business_days"

// CasesConfig is the top-level cases block. It currently carries only the
// missing-settlement SLA. The shared agent/retry attempt cap stays the existing
// constant 3; there is deliberately no cases.max_attempts key.
type CasesConfig struct {
	MissingSettlementSLABusinessDays int `yaml:"missing_settlement_sla_business_days"`
}

// DefaultCasesConfig returns the documented default.
func DefaultCasesConfig() CasesConfig {
	return CasesConfig{MissingSettlementSLABusinessDays: DefaultMissingSettlementSLABusinessDays}
}

// Validate enforces the strict 1..365 range. Values are rejected, never
// clamped, so an operator typo cannot silently change the SLA.
func (c CasesConfig) Validate() error {
	if c.MissingSettlementSLABusinessDays < 1 || c.MissingSettlementSLABusinessDays > 365 {
		return fmt.Errorf("config: %s must be between 1 and 365, got %d",
			MissingSettlementSLAKey, c.MissingSettlementSLABusinessDays)
	}
	return nil
}

// UnmarshalYAML decodes the cases block with an explicit key allow-list. The
// custom decoder exists so a non-integer SLA is reported as a config error that
// names the key and range instead of a raw yaml unmarshal error; unknown keys
// (for example a prohibited cases.max_attempts) are still rejected.
func (c *CasesConfig) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode && node.Tag == "!!null" {
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("config: cases must be a mapping")
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		value := node.Content[i+1]
		switch key {
		case "missing_settlement_sla_business_days":
			if value.Kind != yaml.ScalarNode || value.Tag != "!!int" {
				return fmt.Errorf("config: %s must be an integer between 1 and 365",
					MissingSettlementSLAKey)
			}
			var days int
			if err := value.Decode(&days); err != nil {
				return fmt.Errorf("config: %s must be an integer between 1 and 365",
					MissingSettlementSLAKey)
			}
			c.MissingSettlementSLABusinessDays = days
		default:
			return fmt.Errorf("config: unknown cases key %q", key)
		}
	}
	return nil
}
