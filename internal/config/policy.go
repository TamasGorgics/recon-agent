package config

// Policy file settings. The policy document itself is loaded by
// internal/policy so that the raw bytes can be content-hashed and versioned;
// this package only resolves the path from YAML/environment.
const (
	// EnvPolicyFile names the environment variable that overrides the policy
	// document path.
	EnvPolicyFile = "POLICY_FILE"
	// DefaultPolicyFile is the committed policy document, relative to the
	// repository root.
	DefaultPolicyFile = "policy.yaml"
)

// PolicyConfig names the YAML risk-policy document.
type PolicyConfig struct {
	File string `yaml:"file"`
}
