package proposal

import (
	"bytes"
	"flag"
	"os"
	"strings"
	"testing"
)

var updateGolden = flag.Bool("update", false, "update golden files")

func goldenState() State {
	signatureA := strings.Repeat("a", 64)
	signatureB := strings.Repeat("b", 64)
	return State{
		Case: CaseState{ID: unitCaseID, Status: "open"},
		Breaks: []BreakState{
			{
				ID:           "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
				Signature:    signatureB,
				Side:         "amount_mismatch",
				Status:       "open",
				DeltaMinor:   -250,
				Currency:     "USD",
				LedgerTxnIDs: []string{unitTxnID, "77777777-7777-4777-8777-777777777777"},
				ExternalRefs: []string{"ext:psp_a:rec-2", "ext:psp_a:rec-1"},
			},
			{
				ID:           "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
				Signature:    signatureA,
				Side:         "ledger_only",
				Status:       "in_case",
				DeltaMinor:   1200,
				Currency:     "EUR",
				LedgerTxnIDs: []string{},
				ExternalRefs: []string{"ext:bank_x:statement-9"},
			},
		},
		Records: []RecordState{
			{
				Ref:  "ledger_txn:" + unitTxnID,
				Kind: "ledger_txn",
				Fields: map[string]any{
					"kind":       "charge",
					"value_date": "2026-01-01",
					"entries": []map[string]any{
						{"account_code": "psp_clearing_usd", "direction": "debit", "amount_minor": 1250, "currency": "USD"},
						{"account_code": "revenue_usd", "direction": "credit", "amount_minor": 1250, "currency": "USD"},
					},
				},
			},
			{
				Ref:  "case:" + unitCaseID,
				Kind: "case",
				Fields: map[string]any{
					"id":              unitCaseID,
					"status":          "open",
					"resolution_type": "",
				},
			},
			{
				Ref:  "ext:psp_a:rec-1",
				Kind: "ext",
				Fields: map[string]any{
					"source":           "psp_a",
					"source_record_id": "rec-1",
					"kind":             "charge",
					"amount_minor":     int64(1250),
					"currency":         "USD",
					"fee_minor":        int64(25),
					"value_date":       "2026-01-01",
					"raw_hash":         strings.Repeat("c", 64),
				},
			},
		},
	}
}

// TestCanonicalStateGolden fixes the canonical bytes and hash so M2-08 and
// M4-06 cannot drift silently.
func TestCanonicalStateGolden(t *testing.T) {
	t.Parallel()
	state := goldenState()
	canonical, err := Canonical(state)
	if err != nil {
		t.Fatalf("Canonical(): %v", err)
	}
	hash, err := Hash(state)
	if err != nil {
		t.Fatalf("Hash(): %v", err)
	}
	if len(hash) != 64 || strings.ToLower(hash) != hash {
		t.Fatalf("hash = %q, want 64 lowercase hex chars", hash)
	}

	goldenPath := "testdata/state_golden.json"
	hashPath := "testdata/state_golden.hash"
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("create testdata: %v", err)
		}
		if err := os.WriteFile(goldenPath, append(canonical, '\n'), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		if err := os.WriteFile(hashPath, []byte(hash+"\n"), 0o644); err != nil {
			t.Fatalf("write hash: %v", err)
		}
	}
	wantCanonical, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(canonical, bytes.TrimSuffix(wantCanonical, []byte("\n"))) {
		t.Fatalf("canonical state drifted from %s\n got: %s\nwant: %s", goldenPath, canonical, wantCanonical)
	}
	wantHash, err := os.ReadFile(hashPath)
	if err != nil {
		t.Fatalf("read hash: %v", err)
	}
	if hash != strings.TrimSpace(string(wantHash)) {
		t.Fatalf("hash drifted: got %s, want %s", hash, strings.TrimSpace(string(wantHash)))
	}
}

// TestCanonicalStateSensitivity proves ordering is irrelevant and money is
// part of the hash.
func TestCanonicalStateSensitivity(t *testing.T) {
	t.Parallel()
	state := goldenState()
	base, err := Hash(state)
	if err != nil {
		t.Fatalf("Hash(): %v", err)
	}

	reordered := goldenState()
	reordered.Breaks[0], reordered.Breaks[1] = reordered.Breaks[1], reordered.Breaks[0]
	for i := range reordered.Breaks {
		if reordered.Breaks[i].ID != "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb" {
			continue
		}
		ids := reordered.Breaks[i].LedgerTxnIDs
		reordered.Breaks[i].LedgerTxnIDs = []string{ids[1], ids[0]}
		refs := reordered.Breaks[i].ExternalRefs
		reordered.Breaks[i].ExternalRefs = []string{refs[1], refs[0]}
	}
	reordered.Records[0], reordered.Records[2] = reordered.Records[2], reordered.Records[0]
	reorderedHash, err := Hash(reordered)
	if err != nil {
		t.Fatalf("Hash(reordered): %v", err)
	}
	if base != reorderedHash {
		t.Fatal("break/record/ref ordering changed the hash")
	}

	// Ledger entries arrive in DB row order; reversing them must not change
	// the canonical state. This is the durable projection used by M4-06.
	entriesReordered := goldenState()
	entries := entriesReordered.Records[0].Fields["entries"].([]map[string]any)
	entries[0], entries[1] = entries[1], entries[0]
	entriesHash, err := Hash(entriesReordered)
	if err != nil {
		t.Fatalf("Hash(entriesReordered): %v", err)
	}
	if base != entriesHash {
		t.Fatal("ledger entry order changed the hash")
	}

	moneyChanged := goldenState()
	moneyChanged.Records[0].Fields["entries"] = []map[string]any{
		{"account_code": "psp_clearing_usd", "direction": "debit", "amount_minor": 1251, "currency": "USD"},
		{"account_code": "revenue_usd", "direction": "credit", "amount_minor": 1251, "currency": "USD"},
	}
	moneyHash, err := Hash(moneyChanged)
	if err != nil {
		t.Fatalf("Hash(moneyChanged): %v", err)
	}
	if base == moneyHash {
		t.Fatal("changing a money field did not change the hash")
	}

	deltaChanged := goldenState()
	deltaChanged.Breaks[0].DeltaMinor = -251
	deltaHash, err := Hash(deltaChanged)
	if err != nil {
		t.Fatalf("Hash(deltaChanged): %v", err)
	}
	if base == deltaHash {
		t.Fatal("changing a break delta did not change the hash")
	}
}

// TestCanonicalStateCompositeLegs proves every cited composite leg appears in
// the hashed state and that leg/record order cannot change the hash.
func TestCanonicalStateCompositeLegs(t *testing.T) {
	t.Parallel()
	legs := []string{
		"77777777-7777-4777-8777-777777777777",
		"88888888-8888-4888-8888-888888888888",
		"99999999-9999-4999-8999-999999999998",
	}
	legRecord := func(id string, amount int64) RecordState {
		return RecordState{
			Ref:  "ledger_txn:" + id,
			Kind: "ledger_txn",
			Fields: map[string]any{
				"kind":       "charge",
				"value_date": "2026-01-02",
				"entries": []map[string]any{
					{"account_code": "clearing_usd", "direction": "debit", "amount_minor": amount, "currency": "USD"},
					{"account_code": "revenue_usd", "direction": "credit", "amount_minor": amount, "currency": "USD"},
				},
			},
		}
	}
	base := State{
		Case: CaseState{ID: unitCaseID, Status: "open"},
		Breaks: []BreakState{{
			ID:           "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
			Signature:    strings.Repeat("b", 64),
			Side:         "external_only",
			Status:       "open",
			DeltaMinor:   -3000,
			Currency:     "USD",
			LedgerTxnIDs: []string{},
			ExternalRefs: []string{"ext:psp_a:rec-1"},
		}},
		Records: []RecordState{
			legRecord(legs[0], 1000),
			legRecord(legs[1], 1001),
			legRecord(legs[2], 1002),
			{
				Ref:  "ext:psp_a:rec-1",
				Kind: "ext",
				Fields: map[string]any{
					"source":           "psp_a",
					"source_record_id": "rec-1",
					"amount_minor":     int64(-3000),
					"currency":         "USD",
				},
			},
			{Ref: "case:" + unitCaseID, Kind: "case", Fields: map[string]any{
				"id": unitCaseID, "status": "open", "resolution_type": "",
			}},
		},
	}
	baseHash, err := Hash(base)
	if err != nil {
		t.Fatalf("Hash(base): %v", err)
	}
	canonical, err := Canonical(base)
	if err != nil {
		t.Fatalf("Canonical(base): %v", err)
	}
	for _, leg := range legs {
		if !bytes.Contains(canonical, []byte(`"ledger_txn:`+leg+`"`)) {
			t.Fatalf("canonical state is missing cited leg %s: %s", leg, canonical)
		}
	}

	reordered := base
	reordered.Records = []RecordState{
		base.Records[4], base.Records[2], base.Records[0], base.Records[3], base.Records[1],
	}
	reorderedHash, err := Hash(reordered)
	if err != nil {
		t.Fatalf("Hash(reordered): %v", err)
	}
	if baseHash != reorderedHash {
		t.Fatal("record order changed the composite state hash")
	}

	dropped := base
	dropped.Records = []RecordState{base.Records[0], base.Records[1], base.Records[3], base.Records[4]}
	droppedHash, err := Hash(dropped)
	if err != nil {
		t.Fatalf("Hash(dropped): %v", err)
	}
	if baseHash == droppedHash {
		t.Fatal("dropping a cited composite leg did not change the state hash")
	}
}

func TestEmptyStateIsStable(t *testing.T) {
	t.Parallel()
	empty := State{Case: CaseState{ID: unitCaseID, Status: "open"}}
	canonical, err := Canonical(empty)
	if err != nil {
		t.Fatalf("Canonical(): %v", err)
	}
	if !bytes.Contains(canonical, []byte(`"breaks":[]`)) || !bytes.Contains(canonical, []byte(`"records":[]`)) {
		t.Fatalf("empty state is not stable: %s", canonical)
	}
	hash, err := Hash(empty)
	if err != nil {
		t.Fatalf("Hash(): %v", err)
	}
	if len(hash) != 64 {
		t.Fatalf("hash = %q", hash)
	}
}
