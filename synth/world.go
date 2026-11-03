package synth

import (
	"time"

	"github.com/TamasGorgics/recon-agent/internal/config"
)

// GeneratorVersion is the identity of the generation algorithm. It is part of
// manifest.json and the dataset version. Bump it whenever a code change would
// alter the bytes produced for an unchanged seed and config.
const GeneratorVersion = "1.0.0"

// Source names used by the generated files. They match internal/ingest.
const (
	SourcePSP  = "psp_a"
	SourceBank = "bank_x"
)

// Difficulty levels accepted by the generator. M1-09 generates clean data;
// the level is recorded in the manifest and ground truth so M1-10 can add
// noise without changing the dataset contract.
const (
	DifficultyEasy        = "easy"
	DifficultyRealistic   = "realistic"
	DifficultyAdversarial = "adversarial"
)

// Options selects one deterministic dataset. Config must be validated; zero
// means the caller must pass config.DefaultSynthConfig().
type Options struct {
	Seed       int64
	Days       int
	Difficulty string
	Config     config.SynthConfig
}

// Account is one chart-of-accounts row. IDs are stable UUIDv5 logical IDs, not
// database-generated UUIDs.
type Account struct {
	ID       string `json:"id"`
	Code     string `json:"code"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Currency string `json:"currency"`
}

// Customer is a fake party in the dataset.
type Customer struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Merchant is a fake merchant in the dataset.
type Merchant struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// FeeSchedule is one fee_schedules row produced from the generator config.
// ID is a UUIDv5 logical ID so loading is idempotent.
type FeeSchedule struct {
	ID         string  `json:"id"`
	Source     string  `json:"source"`
	Kind       string  `json:"kind"`
	Currency   string  `json:"currency"`
	PctBps     int     `json:"pct_bps"`
	FixedMinor int64   `json:"fixed_minor"`
	ValidFrom  string  `json:"valid_from"`
	ValidTo    *string `json:"valid_to"`
}

// Scope is one (source, currency) reconciliation scope. LedgerAccountCode names
// the distinct clearing account the scope uses.
type Scope struct {
	Source            string `json:"source"`
	Currency          string `json:"currency"`
	LedgerAccountCode string `json:"ledger_account_code"`
}

// Line is one double-entry line in ledger.json. AmountMinor is always positive
// and Direction carries the sign. AccountCode is included for loaders and
// reviewers; AccountID is the stable logical UUID.
type Line struct {
	AccountID   string         `json:"account_id"`
	AccountCode string         `json:"account_code"`
	Direction   string         `json:"direction"`
	AmountMinor int64          `json:"amount_minor"`
	Currency    string         `json:"currency"`
	ExternalRef string         `json:"external_ref,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

// Transaction is one ledger transaction. CreatedAt and BookedAt are explicit
// RFC 3339 UTC values supplied by the generator; ValueDate is YYYY-MM-DD.
// IdempotencyKey is derived from the stable ID so `synth load` can replay.
type Transaction struct {
	ID             string `json:"id"`
	IdempotencyKey string `json:"idempotency_key"`
	Kind           string `json:"kind"`
	ValueDate      string `json:"value_date"`
	CreatedAt      string `json:"created_at"`
	BookedAt       string `json:"booked_at"`
	Description    string `json:"description"`
	Actor          string `json:"actor"`
	Lines          []Line `json:"lines"`
}

// File is one rendered output file. Path is slash-separated and relative to the
// dataset directory.
type File struct {
	Path string
	Data []byte
}

// Counts summarises the dataset so the manifest, tests, and the loader can
// check the one-to-one ground-truth contract.
type Counts struct {
	Accounts         int `json:"accounts"`
	Customers        int `json:"customers"`
	Merchants        int `json:"merchants"`
	FeeSchedules     int `json:"fee_schedules"`
	LedgerTxns       int `json:"ledger_txns"`
	ExternalRecords  int `json:"external_records"`
	GroundTruthLinks int `json:"ground_truth_links"`
}

// Manifest is manifest.json.
type Manifest struct {
	GeneratorVersion string `json:"generator_version"`
	Seed             int64  `json:"seed"`
	Days             int    `json:"days"`
	Difficulty       string `json:"difficulty"`
	ConfigHash       string `json:"config_hash"`
	DatasetVersion   string `json:"dataset_version"`
	Counts           Counts `json:"counts"`
}

// Amount is one signed amount in ground truth, always integer minor units.
type Amount struct {
	Currency    string `json:"currency"`
	AmountMinor int64  `json:"amount_minor"`
}

// RecordRef identifies one external record by its stable source key.
type RecordRef struct {
	Source         string `json:"source"`
	SourceRecordID string `json:"source_record_id"`
}

// Scenario is one ground-truth row. M1-09 emits clean scenarios: one external
// record linked to exactly one ledger transaction, with resolution "link".
// Ground truth is text-only and never drives policy.
type Scenario struct {
	ScenarioID          string      `json:"scenario_id"`
	Mode                string      `json:"mode"`
	Difficulty          string      `json:"difficulty"`
	BreakSignature      string      `json:"break_signature"`
	ExpectedResolutions []string    `json:"expected_resolutions"`
	PrimaryResolution   string      `json:"primary_resolution"`
	ExpectedTier        string      `json:"expected_tier"`
	Amounts             []Amount    `json:"amounts"`
	Accounts            []string    `json:"accounts"`
	LedgerTxnIDs        []string    `json:"ledger_txn_ids"`
	ExternalRecords     []RecordRef `json:"external_records"`
}

// GroundTruth is ground_truth.json.
type GroundTruth struct {
	DatasetVersion string     `json:"dataset_version"`
	Scenarios      []Scenario `json:"scenarios"`
}

// LedgerDocument is ledger.json. It carries the reference data load needs plus
// the transactions themselves, so generation has no database dependency.
type LedgerDocument struct {
	GeneratorVersion     string        `json:"generator_version"`
	DatasetVersion       string        `json:"dataset_version"`
	Accounts             []Account     `json:"accounts"`
	Customers            []Customer    `json:"customers"`
	Merchants            []Merchant    `json:"merchants"`
	FeeSchedules         []FeeSchedule `json:"fee_schedules"`
	ReconciliationScopes []Scope       `json:"reconciliation_scopes"`
	Transactions         []Transaction `json:"transactions"`
}

// Dataset is the complete in-memory generation result. Write renders it.
type Dataset struct {
	Manifest Manifest
	Ledger   LedgerDocument
	Truth    GroundTruth
	Files    []File
}

// Date layouts. All dates are UTC and all timestamps are RFC 3339 UTC.
const (
	dateLayout      = "2006-01-02"
	timestampLayout = time.RFC3339
)
