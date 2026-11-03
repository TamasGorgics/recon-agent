package match_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/match"
)

// benchmarkFixture builds the M1-13 100k-record benchmark data. Every pass has
// work: 80k exact pairs, 5k tolerance pairs, 2k declared composite payouts
// (4k member records), 5k fuzzy pairs, and 2k duplicate groups (4k records).
// The totals are 100k external records and 94k ledger transactions.
func benchmarkFixture(tb testing.TB) ([]match.ExternalRecord, []match.LedgerTxn) {
	tb.Helper()
	base := uDate("2026-06-01")
	external := make([]match.ExternalRecord, 0, 100_000)
	ledger := make([]match.LedgerTxn, 0, 94_000)

	dateFor := func(i int) time.Time { return base.AddDate(0, 0, i%30) }

	for i := 0; i < 80_000; i++ {
		day := dateFor(i)
		external = append(external, match.ExternalRecord{
			ID: fmt.Sprintf("x-exact-%06d", i), Source: "psp_a",
			SourceRecordID: fmt.Sprintf("X-%06d", i), Kind: "charge",
			AmountMinor: int64(1_000 + i), Currency: "USD", ValueDate: day,
			OccurredAt: day.Add(12 * time.Hour), Reference: fmt.Sprintf("REF-X-%06d", i),
		})
		ledger = append(ledger, match.LedgerTxn{
			ID: fmt.Sprintf("l-exact-%06d", i), AmountMinor: int64(1_000 + i),
			Currency: "USD", ValueDate: day, Reference: fmt.Sprintf("REF-X-%06d", i),
		})
	}
	for i := 0; i < 5_000; i++ {
		day := dateFor(i)
		external = append(external, match.ExternalRecord{
			ID: fmt.Sprintf("x-tol-%06d", i), Source: "psp_a",
			SourceRecordID: fmt.Sprintf("T-%06d", i), Kind: "charge",
			AmountMinor: int64(10_000 + i), Currency: "USD", ValueDate: day,
			OccurredAt: day.Add(12 * time.Hour), Reference: fmt.Sprintf("REF-T-%06d", i),
		})
		ledger = append(ledger, match.LedgerTxn{
			ID: fmt.Sprintf("l-tol-%06d", i), AmountMinor: int64(10_000+i) - 50,
			Currency: "USD", ValueDate: day, Reference: fmt.Sprintf("REF-T-%06d", i),
		})
	}
	for i := 0; i < 2_000; i++ {
		day := dateFor(i)
		first, second := int64(6_000+i), int64(4_000+i)
		external = append(external, match.ExternalRecord{
			ID: fmt.Sprintf("x-comp-%06d", i), Source: "psp_a",
			SourceRecordID: fmt.Sprintf("P-%06d", i), Kind: "payout",
			AmountMinor: -(first + second), Currency: "USD", ValueDate: day,
			OccurredAt: day.Add(12 * time.Hour), Batch: fmt.Sprintf("B-%06d", i),
		})
		for member, amount := range []int64{1, 2} {
			ref := fmt.Sprintf("MR-%06d-%d", i, member)
			external = append(external, match.ExternalRecord{
				ID: fmt.Sprintf("x-cmem-%06d-%d", i, member), Source: "psp_a",
				SourceRecordID: fmt.Sprintf("M-%06d-%d", i, member), Kind: "charge",
				AmountMinor: amount, Currency: "USD", ValueDate: day,
				OccurredAt: day.Add(time.Duration(13+member)*time.Hour + time.Duration(i%3600)*time.Second),
				Reference:  ref, Batch: fmt.Sprintf("B-%06d", i),
			})
		}
		ledger = append(ledger,
			match.LedgerTxn{
				ID: fmt.Sprintf("l-comp-%06d-1", i), AmountMinor: -first,
				Currency: "USD", ValueDate: day, Reference: fmt.Sprintf("MR-%06d-0", i),
			},
			match.LedgerTxn{
				ID: fmt.Sprintf("l-comp-%06d-2", i), AmountMinor: -second,
				Currency: "USD", ValueDate: day, Reference: fmt.Sprintf("MR-%06d-1", i),
			},
		)
	}
	for i := 0; i < 5_000; i++ {
		day := dateFor(i)
		external = append(external, match.ExternalRecord{
			ID: fmt.Sprintf("x-fuzzy-%06d", i), Source: "psp_a",
			SourceRecordID: fmt.Sprintf("F-%06d", i), Kind: "charge",
			AmountMinor: int64(2_000 + i), Currency: "USD", ValueDate: day,
			OccurredAt: day.Add(12 * time.Hour), Reference: fmt.Sprintf("FEXT-%06d", i),
		})
		ledger = append(ledger, match.LedgerTxn{
			ID: fmt.Sprintf("l-fuzzy-%06d", i), AmountMinor: int64(2_000 + i),
			Currency: "USD", ValueDate: day, Reference: fmt.Sprintf("FLED-%06d", i),
		})
	}
	for i := 0; i < 2_000; i++ {
		day := dateFor(i)
		for duplicate := 0; duplicate < 2; duplicate++ {
			external = append(external, match.ExternalRecord{
				ID: fmt.Sprintf("x-dup-%06d-%d", i, duplicate), Source: "psp_a",
				SourceRecordID: fmt.Sprintf("D-%06d-%d", i, duplicate), Kind: "charge",
				AmountMinor: int64(100_000 + i), Currency: "USD", ValueDate: day,
				OccurredAt: day.Add(10 * time.Hour),
			})
		}
	}

	if len(external) != 100_000 {
		tb.Fatalf("benchmark fixture external records = %d, want 100000", len(external))
	}
	if len(ledger) != 94_000 {
		tb.Fatalf("benchmark fixture ledger txns = %d, want 94000", len(ledger))
	}
	return external, ledger
}

// BenchmarkMatch100k is the M1-13 acceptance benchmark: 100k external records
// in one scope through the complete pass chain. Run with
// `go test -run '^$' -bench BenchmarkMatch100k -benchmem -benchtime=1x ./internal/match`.
func BenchmarkMatch100k(b *testing.B) {
	external, ledger := benchmarkFixture(b)
	cfg := unitConfig()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		candidates, breaks, counts, err := match.EvaluateWithBreaks(
			b.Context(),
			match.NewRunContext(unitScope, unitWindow, uTime("2026-06-15T12:00:00Z"), external, ledger),
			match.DefaultPasses(cfg))
		if err != nil {
			b.Fatalf("evaluate 100k: %v", err)
		}
		if got, want := len(candidates), 92_000; got != want {
			b.Fatalf("matched = %d, want %d (counts = %v)", got, want, counts)
		}
		if got, want := len(breaks), 2_000; got != want {
			b.Fatalf("breaks = %d, want %d", got, want)
		}
	}
}
