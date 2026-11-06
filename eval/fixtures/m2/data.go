// Package m2 owns the deterministic three-scenario fixture used by the M2
// smoke tests, cassette recordings, and the M2 exit gate. It is eval-only: the
// production demo seed path is unchanged.
package m2

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// FixedInstant is the single instant every fixture timestamp derives from.
// Changing it changes the tool transcripts and therefore requires re-recording
// the cassettes.
var FixedInstant = time.Date(2026, time.January, 15, 0, 0, 0, 0, time.UTC)

// FixedPeriodID is the broad open period seeded by the M1-03 migration.
const FixedPeriodID = "11111111-1111-4111-8111-111111111111"

// Scenario names and expected proposal types from the locked M2 defaults.
const (
	ScenarioFeeVariance   = "fee_variance"
	ScenarioReferenceTypo = "reference_typo"
	ScenarioDuplicate     = "duplicate_charge"
)

// Scenario is one row of the fixed smoke manifest.
type Scenario struct {
	CaseID       string
	Mode         string
	ExpectedType string
}

// Scenarios returns the fixed three-case manifest in deterministic order.
func Scenarios() []Scenario {
	return []Scenario{
		{
			CaseID:       FeeVarianceCaseID,
			Mode:         ScenarioFeeVariance,
			ExpectedType: "adjustment",
		},
		{
			CaseID:       ReferenceTypoCaseID,
			Mode:         ScenarioReferenceTypo,
			ExpectedType: "link",
		},
		{
			CaseID:       DuplicateChargeCaseID,
			Mode:         ScenarioDuplicate,
			ExpectedType: "duplicate",
		},
	}
}

// Fixed case ids. They are constants because the cassette request transcript
// embeds them.
const (
	FeeVarianceCaseID     = "10000000-0000-4000-8000-000000000001"
	ReferenceTypoCaseID   = "10000000-0000-4000-8000-000000000002"
	DuplicateChargeCaseID = "10000000-0000-4000-8000-000000000003"
)

const (
	fixtureSource     = "psp_m2"
	fixtureCurrency   = "USD"
	fixtureAccountUSD = "m2_clearing_usd"
)

type accountRow struct {
	ID       string
	Code     string
	Name     string
	Type     string
	Currency string
	Status   string
}

type txnRow struct {
	ID             string
	Kind           string
	IdempotencyKey string
	RequestHash    string
	Description    string
	ValueDate      string
	CreatedAt      time.Time
	BookedAt       time.Time
}

type entryRow struct {
	ID          string
	TxnID       string
	AccountID   string
	Direction   string
	AmountMinor int64
	Currency    string
	PostedAt    time.Time
	ExternalRef string
}

type externalRecordRow struct {
	ID             string
	SourceRecordID string
	Kind           string
	AmountMinor    int64
	FeeMinor       int64
	OccurredAt     time.Time
	SettledAt      time.Time
	ValueDate      string
	Raw            map[string]string
}

type breakRef struct {
	Source         string `json:"source"`
	SourceRecordID string `json:"source_record_id"`
}

type breakRow struct {
	ID           string
	Signature    string
	Side         string
	LedgerTxnIDs []string
	ExternalRefs []breakRef
	DeltaMinor   int64
	Currency     string
	Status       string
	DetectedAt   time.Time
}

type caseRow struct {
	ID         string
	Priority   int
	ValueMinor int64
	Currency   string
	Status     string
	Version    int
	Attempts   int
	CreatedAt  time.Time
	BreakID    string
}

type fixtureWorld struct {
	Accounts        []accountRow
	Txns            []txnRow
	Entries         []entryRow
	ExternalRecords []externalRecordRow
	Breaks          []breakRow
	Cases           []caseRow
}

func signature(label string) string {
	sum := sha256.Sum256([]byte(label))
	return hex.EncodeToString(sum[:])
}

func rawJSON(values map[string]string) string {
	encoded, err := json.Marshal(values)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func buildWorld() fixtureWorld {
	const (
		clearingID = "a0000000-0000-4000-8000-000000000001"
		feesID     = "a0000000-0000-4000-8000-000000000003"
		suspenseID = "a0000000-0000-4000-8000-000000000004"
	)
	return fixtureWorld{
		Accounts: []accountRow{
			{ID: clearingID, Code: fixtureAccountUSD, Name: "M2 settlement clearing", Type: "asset", Currency: fixtureCurrency, Status: "active"},
			{ID: feesID, Code: "fees_expense", Name: "M2 fees expense", Type: "expense", Currency: fixtureCurrency, Status: "active"},
			{ID: suspenseID, Code: "suspense", Name: "M2 suspense", Type: "liability", Currency: fixtureCurrency, Status: "active"},
		},
		Txns: []txnRow{
			{
				ID: "c0000000-0000-4000-8000-000000000001", Kind: "charge",
				IdempotencyKey: "m2-fee-variance-txn", RequestHash: signature("m2-fee-variance-txn"),
				Description: "Synthetic monthly processing charge",
				ValueDate:   "2026-01-15", CreatedAt: FixedInstant, BookedAt: FixedInstant,
			},
			{
				ID: "c0000000-0000-4000-8000-000000000002", Kind: "charge",
				IdempotencyKey: "m2-reference-typo-txn", RequestHash: signature("m2-reference-typo-txn"),
				Description: "Synthetic subscription charge",
				ValueDate:   "2026-01-15", CreatedAt: FixedInstant, BookedAt: FixedInstant,
			},
			{
				ID: "c0000000-0000-4000-8000-000000000003", Kind: "charge",
				IdempotencyKey: "m2-duplicate-charge-txn", RequestHash: signature("m2-duplicate-charge-txn"),
				Description: "Synthetic duplicate charge",
				ValueDate:   "2026-01-15", CreatedAt: FixedInstant, BookedAt: FixedInstant,
			},
		},
		Entries: []entryRow{
			{ID: "e0000000-0000-4000-8000-000000000001", TxnID: "c0000000-0000-4000-8000-000000000001", AccountID: clearingID, Direction: "debit", AmountMinor: 10000, Currency: fixtureCurrency, PostedAt: FixedInstant, ExternalRef: "fee-1"},
			{ID: "e0000000-0000-4000-8000-000000000002", TxnID: "c0000000-0000-4000-8000-000000000001", AccountID: feesID, Direction: "debit", AmountMinor: 30, Currency: fixtureCurrency, PostedAt: FixedInstant, ExternalRef: "fee-1"},
			{ID: "e0000000-0000-4000-8000-000000000003", TxnID: "c0000000-0000-4000-8000-000000000001", AccountID: suspenseID, Direction: "credit", AmountMinor: 10030, Currency: fixtureCurrency, PostedAt: FixedInstant, ExternalRef: "fee-1"},
			{ID: "e0000000-0000-4000-8000-000000000004", TxnID: "c0000000-0000-4000-8000-000000000002", AccountID: clearingID, Direction: "debit", AmountMinor: 5000, Currency: fixtureCurrency, PostedAt: FixedInstant, ExternalRef: "ref-l00"},
			{ID: "e0000000-0000-4000-8000-000000000005", TxnID: "c0000000-0000-4000-8000-000000000002", AccountID: suspenseID, Direction: "credit", AmountMinor: 5000, Currency: fixtureCurrency, PostedAt: FixedInstant, ExternalRef: "ref-l00"},
			{ID: "e0000000-0000-4000-8000-000000000006", TxnID: "c0000000-0000-4000-8000-000000000003", AccountID: clearingID, Direction: "debit", AmountMinor: 2500, Currency: fixtureCurrency, PostedAt: FixedInstant, ExternalRef: "dup-1"},
			{ID: "e0000000-0000-4000-8000-000000000007", TxnID: "c0000000-0000-4000-8000-000000000003", AccountID: suspenseID, Direction: "credit", AmountMinor: 2500, Currency: fixtureCurrency, PostedAt: FixedInstant, ExternalRef: "dup-1"},
		},
		ExternalRecords: []externalRecordRow{
			{ID: "f0000000-0000-4000-8000-000000000001", SourceRecordID: "fee-1", Kind: "charge", AmountMinor: 10000, FeeMinor: 20, OccurredAt: FixedInstant, SettledAt: FixedInstant, ValueDate: "2026-01-15", Raw: map[string]string{"reference": "fee-1", "memo": "Synthetic monthly processing charge"}},
			{ID: "f0000000-0000-4000-8000-000000000002", SourceRecordID: "ref-100", Kind: "charge", AmountMinor: 5000, FeeMinor: 0, OccurredAt: FixedInstant, SettledAt: FixedInstant, ValueDate: "2026-01-15", Raw: map[string]string{"reference": "ref-100", "memo": "Synthetic subscription charge"}},
			{ID: "f0000000-0000-4000-8000-000000000003", SourceRecordID: "dup-1", Kind: "charge", AmountMinor: 2500, FeeMinor: 0, OccurredAt: FixedInstant, SettledAt: FixedInstant, ValueDate: "2026-01-15", Raw: map[string]string{"reference": "dup-1", "memo": "Synthetic duplicate candidate"}},
			{ID: "f0000000-0000-4000-8000-000000000004", SourceRecordID: "dup-2", Kind: "charge", AmountMinor: 2500, FeeMinor: 0, OccurredAt: FixedInstant, SettledAt: FixedInstant, ValueDate: "2026-01-15", Raw: map[string]string{"reference": "dup-2", "memo": "Synthetic duplicate candidate"}},
		},
		Breaks: []breakRow{
			{
				ID: "b0000000-0000-4000-8000-000000000001", Signature: signature("m2-fee-variance-break"),
				Side: "amount_mismatch", LedgerTxnIDs: []string{"c0000000-0000-4000-8000-000000000001"},
				ExternalRefs: []breakRef{{Source: fixtureSource, SourceRecordID: "fee-1"}},
				DeltaMinor:   10, Currency: fixtureCurrency, Status: "in_case", DetectedAt: FixedInstant,
			},
			{
				ID: "b0000000-0000-4000-8000-000000000002", Signature: signature("m2-reference-typo-break"),
				Side: "amount_mismatch", LedgerTxnIDs: []string{"c0000000-0000-4000-8000-000000000002"},
				ExternalRefs: []breakRef{{Source: fixtureSource, SourceRecordID: "ref-100"}},
				DeltaMinor:   0, Currency: fixtureCurrency, Status: "in_case", DetectedAt: FixedInstant,
			},
			{
				ID: "b0000000-0000-4000-8000-000000000003", Signature: signature("m2-duplicate-charge-break"),
				Side: "duplicate", LedgerTxnIDs: []string{"c0000000-0000-4000-8000-000000000003"},
				ExternalRefs: []breakRef{
					{Source: fixtureSource, SourceRecordID: "dup-1"},
					{Source: fixtureSource, SourceRecordID: "dup-2"},
				},
				DeltaMinor: 2500, Currency: fixtureCurrency, Status: "in_case", DetectedAt: FixedInstant,
			},
		},
		Cases: []caseRow{
			{ID: FeeVarianceCaseID, Priority: 30, ValueMinor: 10, Currency: fixtureCurrency, Status: "open", CreatedAt: FixedInstant, BreakID: "b0000000-0000-4000-8000-000000000001"},
			{ID: ReferenceTypoCaseID, Priority: 20, ValueMinor: 5000, Currency: fixtureCurrency, Status: "open", CreatedAt: FixedInstant, BreakID: "b0000000-0000-4000-8000-000000000002"},
			{ID: DuplicateChargeCaseID, Priority: 10, ValueMinor: 2500, Currency: fixtureCurrency, Status: "open", CreatedAt: FixedInstant, BreakID: "b0000000-0000-4000-8000-000000000003"},
		},
	}
}

var world = buildWorld()
