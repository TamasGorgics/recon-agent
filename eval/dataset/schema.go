package dataset

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/TamasGorgics/recon-agent/synth"
)

// ErrUnsupportedSchemaVersion reports a ground-truth document whose version the
// builder/scorer does not understand. It is a hard error rather than a guess.
var ErrUnsupportedSchemaVersion = errors.New("eval/dataset: unsupported ground-truth schema version")

// SchemaVersion is the current ground-truth schema version. It is an alias for
// TruthSchemaVersion so callers can use either conventional name.
const SchemaVersion = TruthSchemaVersion

//go:embed schema/ground_truth.schema.json
var groundTruthSchemaJSON []byte

var (
	schemaOnce sync.Once
	schema     *jsonschema.Schema
	schemaErr  error
)

func compiledGroundTruthSchema() (*jsonschema.Schema, error) {
	schemaOnce.Do(func() {
		document, err := jsonschema.UnmarshalJSON(bytes.NewReader(groundTruthSchemaJSON))
		if err != nil {
			schemaErr = fmt.Errorf("eval/dataset: parse ground-truth schema: %w", err)
			return
		}
		compiler := jsonschema.NewCompiler()
		compiler.DefaultDraft(jsonschema.Draft2020)
		const location = "https://recon.local/schemas/eval/ground_truth.v1.json"
		if err := compiler.AddResource(location, document); err != nil {
			schemaErr = fmt.Errorf("eval/dataset: add ground-truth schema: %w", err)
			return
		}
		schema, schemaErr = compiler.Compile(location)
		if schemaErr != nil {
			schemaErr = fmt.Errorf("eval/dataset: compile ground-truth schema: %w", schemaErr)
		}
	})
	return schema, schemaErr
}

// ValidateGroundTruth validates a document against schema v1 and the semantic
// rules the JSON Schema cannot express (allowed-resolution membership, required
// expected payloads, and injection override).
func ValidateGroundTruth(truth GroundTruth) error {
	if truth.SchemaVersion != TruthSchemaVersion {
		return fmt.Errorf("%w: %q, want %q",
			ErrUnsupportedSchemaVersion, truth.SchemaVersion, TruthSchemaVersion)
	}
	data, err := json.Marshal(truth)
	if err != nil {
		return fmt.Errorf("eval/dataset: marshal ground truth: %w", err)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("eval/dataset: decode ground truth for validation: %w", err)
	}
	compiled, err := compiledGroundTruthSchema()
	if err != nil {
		return err
	}
	if err := compiled.Validate(instance); err != nil {
		return fmt.Errorf("eval/dataset: ground truth does not match schema v%s: %w",
			TruthSchemaVersion, err)
	}
	return validateGroundTruthSemantics(truth)
}

// LoadGroundTruth reads and validates one ground-truth document. Unknown schema
// versions fail before any scenario is interpreted.
func LoadGroundTruth(path string) (GroundTruth, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return GroundTruth{}, fmt.Errorf("eval/dataset: read ground truth: %w", err)
	}
	var truth GroundTruth
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&truth); err != nil {
		return GroundTruth{}, fmt.Errorf("eval/dataset: decode ground truth %s: %w", path, err)
	}
	if err := ValidateGroundTruth(truth); err != nil {
		return GroundTruth{}, fmt.Errorf("eval/dataset: ground truth %s: %w", path, err)
	}
	return truth, nil
}

func validateGroundTruthSemantics(truth GroundTruth) error {
	if truth.GeneratorDatasetVersion == "" {
		return errors.New("generator_dataset_version is required")
	}
	seen := make(map[string]struct{}, len(truth.Scenarios))
	for index, scenario := range truth.Scenarios {
		where := fmt.Sprintf("scenario %d (%s)", index, scenario.ScenarioID)
		if scenario.ScenarioID == "" {
			return fmt.Errorf("%s: scenario_id is required", where)
		}
		if _, duplicate := seen[scenario.ScenarioID]; duplicate {
			return fmt.Errorf("%s: duplicate scenario_id", where)
		}
		seen[scenario.ScenarioID] = struct{}{}
		if scenario.Mode == "" {
			return fmt.Errorf("%s: mode is required", where)
		}
		switch scenario.ExpectedTier {
		case "T0", "T1", "T2":
		default:
			return fmt.Errorf("%s: expected_tier %q is invalid", where, scenario.ExpectedTier)
		}
		if len(scenario.ExpectedResolutions) == 0 {
			return fmt.Errorf("%s: expected_resolutions is empty", where)
		}
		for _, resolution := range scenario.ExpectedResolutions {
			if !validResolution(resolution) {
				return fmt.Errorf("%s: expected resolution %q is invalid", where, resolution)
			}
		}
		if !contains(scenario.ExpectedResolutions, scenario.PrimaryResolution) {
			return fmt.Errorf("%s: primary_resolution %q is not in expected_resolutions",
				where, scenario.PrimaryResolution)
		}
		if scenario.ExpectedLink != nil {
			if len(scenario.ExpectedLink.LedgerTxnIDs) == 0 ||
				len(scenario.ExpectedLink.ExternalRecords) == 0 {
				return fmt.Errorf("%s: expected_link requires both ledger and external refs", where)
			}
			if err := requireSortedUnique(where+": expected_link.ledger_txn_ids",
				scenario.ExpectedLink.LedgerTxnIDs); err != nil {
				return err
			}
			refs := make([]string, 0, len(scenario.ExpectedLink.ExternalRecords))
			for _, record := range scenario.ExpectedLink.ExternalRecords {
				refs = append(refs, externalRef(record))
			}
			if err := requireSortedUnique(where+": expected_link.external_records", refs); err != nil {
				return err
			}
		}
		if contains(scenario.ExpectedResolutions, "adjustment") && scenario.ExpectedAdjustment == nil {
			return fmt.Errorf("%s: adjustment is allowed but expected_adjustment is missing", where)
		}
		if scenario.ExpectedAdjustment != nil {
			if err := validateAdjustmentLines(where, scenario.ExpectedAdjustment); err != nil {
				return err
			}
		}
		if scenario.Mode == "duplicate_charge" {
			if scenario.ExpectedDuplicate == nil {
				return fmt.Errorf("%s: duplicate_charge is missing expected_duplicate", where)
			}
			if err := validateDuplicate(where, scenario.ExpectedDuplicate); err != nil {
				return err
			}
		}
		if scenario.Mode == "injection_memo" {
			if scenario.Injection == nil {
				return fmt.Errorf("%s: injection_memo is missing injection truth", where)
			}
			if scenario.Injection.ExpectedSafeOutcome != "escalate" {
				return fmt.Errorf("%s: injection expected_safe_outcome %q, want escalate",
					where, scenario.Injection.ExpectedSafeOutcome)
			}
			if len(scenario.ExpectedResolutions) != 1 || scenario.ExpectedResolutions[0] != "escalate" ||
				scenario.PrimaryResolution != "escalate" {
				return fmt.Errorf("%s: injection truth must be exactly [escalate] with primary escalate", where)
			}
		}
	}
	return nil
}

func validateAdjustmentLines(where string, adjustment *ExpectedAdjustment) error {
	if len(adjustment.Lines) == 0 {
		return fmt.Errorf("%s: expected_adjustment has no lines", where)
	}
	debits := make(map[string]int64)
	credits := make(map[string]int64)
	for _, line := range adjustment.Lines {
		if line.AccountCode == "" {
			return fmt.Errorf("%s: adjustment account_code is empty", where)
		}
		if line.Direction != "debit" && line.Direction != "credit" {
			return fmt.Errorf("%s: adjustment direction %q is invalid", where, line.Direction)
		}
		if line.AmountMinor <= 0 {
			return fmt.Errorf("%s: adjustment amount_minor must be positive", where)
		}
		if line.Currency == "" {
			return fmt.Errorf("%s: adjustment currency is empty", where)
		}
		if line.Direction == "debit" {
			debits[line.Currency] += line.AmountMinor
		} else {
			credits[line.Currency] += line.AmountMinor
		}
	}
	for currency, debit := range debits {
		if credits[currency] != debit {
			return fmt.Errorf("%s: adjustment does not balance in %s", where, currency)
		}
	}
	for currency := range credits {
		if _, ok := debits[currency]; !ok {
			return fmt.Errorf("%s: adjustment does not balance in %s", where, currency)
		}
	}
	return nil
}

func validateDuplicate(where string, duplicate *ExpectedDuplicate) error {
	if duplicate.ExternalRef == "" || duplicate.DuplicateOf == "" {
		return fmt.Errorf("%s: expected_duplicate requires external_ref and duplicate_of", where)
	}
	if duplicate.ExternalRef == duplicate.DuplicateOf {
		return fmt.Errorf("%s: expected_duplicate refs must differ", where)
	}
	for _, ref := range []string{duplicate.ExternalRef, duplicate.DuplicateOf} {
		if _, _, err := splitExternalRef(ref); err != nil {
			return fmt.Errorf("%s: expected_duplicate ref %q: %w", where, ref, err)
		}
	}
	return nil
}

func requireSortedUnique(where string, values []string) error {
	for index := 1; index < len(values); index++ {
		if values[index-1] >= values[index] {
			return fmt.Errorf("%s is not sorted and unique at index %d", where, index)
		}
	}
	return nil
}

func validResolution(resolution string) bool {
	switch resolution {
	case "link", "adjustment", "write_off", "duplicate", "request_info", "escalate":
		return true
	default:
		return false
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func splitExternalRef(ref string) (source, recordID string, err error) {
	parts := strings.SplitN(ref, ":", 3)
	if len(parts) != 3 || parts[0] != "ext" || parts[1] == "" || parts[2] == "" {
		return "", "", errors.New("ref is not ext:<source>:<record_id>")
	}
	return parts[1], parts[2], nil
}

func externalRef(record synth.RecordRef) string {
	return "ext:" + record.Source + ":" + record.SourceRecordID
}
