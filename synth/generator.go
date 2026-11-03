package synth

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/money"
)

// maxDays bounds one generation so a typo cannot render an unbounded dataset.
const maxDays = 3_660

// Generator produces deterministic in-memory datasets. It never reads the wall
// clock during Generate: the injected clock exists for Load, which needs audit
// timestamps. Generation must not require Postgres (D-7).
type Generator struct {
	Clock clock.Clock
}

// New returns a generator. The clock must be non-nil; generation output never
// depends on it.
func New(clk clock.Clock) *Generator {
	return &Generator{Clock: clk}
}

// Generate builds one dataset. It is a pure function of (seed, days,
// difficulty, config); no file is written until Dataset.Write is called.
func (g *Generator) Generate(opts Options) (*Dataset, error) {
	if g == nil || g.Clock == nil {
		return nil, errors.New("synth: generator requires an injected clock")
	}
	if opts.Days <= 0 || opts.Days > maxDays {
		return nil, fmt.Errorf("synth: days must be between 1 and %d", maxDays)
	}
	switch opts.Difficulty {
	case DifficultyEasy, DifficultyRealistic, DifficultyAdversarial:
	default:
		return nil, fmt.Errorf("synth: difficulty %q is not easy, realistic, or adversarial", opts.Difficulty)
	}
	if len(opts.Config.Currencies) == 0 {
		opts.Config = config.DefaultSynthConfig()
	}
	if err := opts.Config.Validate(); err != nil {
		return nil, err
	}
	for _, code := range opts.Config.Currencies {
		if _, err := money.ParseCurrency(code); err != nil {
			return nil, fmt.Errorf("synth: currency %q: %w", code, err)
		}
	}
	base, err := time.ParseInLocation(dateLayout, opts.Config.BaseDate, time.UTC)
	if err != nil {
		return nil, fmt.Errorf("synth: base date %q: %w", opts.Config.BaseDate, err)
	}

	configHash := hashHex(opts.Config.Canonical(opts.Days, opts.Difficulty))
	version := datasetVersion(opts.Seed, configHash)

	b := &builder{
		opts:          opts,
		cfg:           opts.Config,
		baseDate:      base,
		configHash:    configHash,
		dataset:       version,
		rng:           rand.New(rand.NewPCG(uint64(opts.Seed), uint64(opts.Seed)^0x9e3779b97f4a7c15)),
		accountByCode: make(map[string]Account),
		opening:       make(map[string]int64),
	}
	if err := b.build(); err != nil {
		return nil, err
	}
	return b.finish()
}

// builder holds one generation pass. All maps are only used for lookups; all
// rendered slices are populated in sorted order so map iteration cannot leak
// into the output.
type builder struct {
	opts       Options
	cfg        config.SynthConfig
	baseDate   time.Time
	configHash string
	dataset    string
	rng        *rand.Rand

	accounts      []Account
	accountByCode map[string]Account
	customers     []Customer
	merchants     []Merchant
	schedules     []FeeSchedule
	scopes        []Scope

	transactions []Transaction
	scenarios    []Scenario

	pspBatches     []pspBatch
	bankStatements []bankStatement
	opening        map[string]int64

	nextTxn       int
	externalCount int
}

func (b *builder) build() error {
	b.buildAccounts()
	b.buildParties()
	b.buildFeeSchedules()
	b.buildScopes()
	for day := 0; day < b.opts.Days; day++ {
		date := b.baseDate.AddDate(0, 0, day)
		for _, currency := range b.cfg.CurrenciesSorted() {
			if err := b.buildCurrencyDay(date, day, currency); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *builder) buildAccounts() {
	for _, currency := range b.cfg.CurrenciesSorted() {
		lower := strings.ToLower(currency)
		b.addAccount("clearing_"+SourcePSP+"_"+lower, "PSP A clearing "+currency, "asset", currency)
		b.addAccount("clearing_"+SourceBank+"_"+lower, "Bank X clearing "+currency, "asset", currency)
		b.addAccount("payout_in_transit_"+lower, "Payout in transit "+currency, "asset", currency)
		b.addAccount("revenue_"+lower, "Card revenue "+currency, "revenue", currency)
		b.addAccount("refunds_expense_"+lower, "Refunds "+currency, "expense", currency)
		b.addAccount("fees_expense_"+lower, "Fees "+currency, "expense", currency)
		b.addAccount("chargebacks_expense_"+lower, "Chargebacks "+currency, "expense", currency)
	}
	sort.Slice(b.accounts, func(i, j int) bool { return b.accounts[i].Code < b.accounts[j].Code })
}

func (b *builder) addAccount(code, name, accountType, currency string) {
	account := Account{
		ID:       logicalID(b.opts.Seed, "account", code),
		Code:     code,
		Name:     name,
		Type:     accountType,
		Currency: currency,
	}
	b.accounts = append(b.accounts, account)
	b.accountByCode[code] = account
}

func (b *builder) buildParties() {
	firstNames := []string{"Avery", "Bryn", "Cato", "Dara", "Ellis", "Faye", "Galen", "Hale", "Iris", "Jory", "Kai", "Lena"}
	lastNames := []string{"Sampleton", "Northmere", "Quorrel", "Vantree", "Westerby", "Yarrow", "Zephyr", "Brookline", "Cedar", "Dunmore", "Everley", "Fenwick"}
	for i := 0; i < b.cfg.Customers; i++ {
		name := fmt.Sprintf("%s %s", firstNames[i%len(firstNames)], lastNames[(i/len(firstNames))%len(lastNames)])
		b.customers = append(b.customers, Customer{
			ID:   logicalID(b.opts.Seed, "customer", strconv.Itoa(i)),
			Name: name,
		})
	}
	merchantNames := []string{"Northwind Synthetic", "Harbor Demo Goods", "Pinegrove Test Market", "Silverline Sample Co"}
	for i := 0; i < b.cfg.Merchants; i++ {
		b.merchants = append(b.merchants, Merchant{
			ID:   logicalID(b.opts.Seed, "merchant", strconv.Itoa(i)),
			Name: merchantNames[i%len(merchantNames)],
		})
	}
}

func (b *builder) buildFeeSchedules() {
	validFrom := b.baseDate.Format(dateLayout)
	for _, currency := range b.cfg.CurrenciesSorted() {
		b.schedules = append(b.schedules, FeeSchedule{
			ID:         feeScheduleID(b.opts.Seed, b.dataset, SourcePSP, "charge", currency, validFrom),
			Source:     SourcePSP,
			Kind:       "charge",
			Currency:   currency,
			PctBps:     b.cfg.FeeBps[currency],
			FixedMinor: b.cfg.FixedFeeMinor[currency],
			ValidFrom:  validFrom,
		})
		b.schedules = append(b.schedules, FeeSchedule{
			ID:         feeScheduleID(b.opts.Seed, b.dataset, SourceBank, "fee", currency, validFrom),
			Source:     SourceBank,
			Kind:       "fee",
			Currency:   currency,
			PctBps:     0,
			FixedMinor: b.cfg.BankFeeMinor[currency],
			ValidFrom:  validFrom,
		})
	}
	sort.Slice(b.schedules, func(i, j int) bool {
		if b.schedules[i].Source != b.schedules[j].Source {
			return b.schedules[i].Source < b.schedules[j].Source
		}
		if b.schedules[i].Kind != b.schedules[j].Kind {
			return b.schedules[i].Kind < b.schedules[j].Kind
		}
		return b.schedules[i].Currency < b.schedules[j].Currency
	})
}

func (b *builder) buildScopes() {
	for _, currency := range b.cfg.CurrenciesSorted() {
		lower := strings.ToLower(currency)
		b.scopes = append(b.scopes,
			Scope{Source: SourcePSP, Currency: currency, LedgerAccountCode: "clearing_" + SourcePSP + "_" + lower},
			Scope{Source: SourceBank, Currency: currency, LedgerAccountCode: "clearing_" + SourceBank + "_" + lower},
		)
	}
	sort.Slice(b.scopes, func(i, j int) bool {
		if b.scopes[i].Source != b.scopes[j].Source {
			return b.scopes[i].Source < b.scopes[j].Source
		}
		return b.scopes[i].Currency < b.scopes[j].Currency
	})
}

func (b *builder) buildCurrencyDay(date time.Time, day int, currency string) error {
	batchID := fmt.Sprintf("B-%s-%s", date.Format("20060102"), currency)
	compact := date.Format("20060102")
	lower := strings.ToLower(currency)
	exponent := money.MustParseCurrency(currency).Exponent()

	chargeSchedule, ok := scheduleFor(b.schedules, SourcePSP, "charge", currency)
	if !ok {
		return fmt.Errorf("synth: no charge fee schedule for %s", currency)
	}

	var rows []pspRow
	var chargeNetTotal int64
	for n := 0; n < b.cfg.ChargesPerDay; n++ {
		customer := b.customers[b.rng.IntN(len(b.customers))]
		merchant := b.merchants[b.rng.IntN(len(b.merchants))]
		gross := b.chargeAmount(currency)
		fee := feeMinor(gross, chargeSchedule.PctBps, chargeSchedule.FixedMinor)
		if fee >= gross {
			fee = gross / 10
		}
		net := gross - fee
		recordID := fmt.Sprintf("psp_%s_%s_c%02d", compact, currency, n+1)
		reference := "REF-" + recordID
		occurred := time.Date(date.Year(), date.Month(), date.Day(), 10, n, 0, 0, time.UTC)
		metadata := map[string]any{
			"source":           SourcePSP,
			"source_record_id": recordID,
			"customer_id":      customer.ID,
			"merchant_id":      merchant.ID,
		}
		rows = append(rows, pspRow{
			RecordID:   recordID,
			Type:       "charge",
			OccurredAt: occurred,
			SettledAt:  &occurred,
			GrossMinor: gross,
			FeeMinor:   fee,
			NetMinor:   net,
			Currency:   currency,
			Reference:  reference,
			BatchID:    batchID,
			Memo:       "synthetic charge for " + customer.Name,
		})
		lines := []Line{
			b.line("clearing_"+SourcePSP+"_"+lower, "debit", gross, currency, reference, metadata),
			b.line("revenue_"+lower, "credit", gross, currency, "", nil),
			b.line("fees_expense_"+lower, "debit", fee, currency, "", nil),
			b.line("clearing_"+SourcePSP+"_"+lower, "credit", fee, currency, "", nil),
		}
		b.addTxn("charge", "synthetic charge "+recordID, date, occurred,
			SourcePSP, recordID, net, lines)
		chargeNetTotal += net
	}

	if day%2 == 1 {
		amount := b.refundAmount(currency, exponent)
		recordID := fmt.Sprintf("psp_%s_%s_r01", compact, currency)
		reference := "REF-" + recordID
		refunded := time.Date(date.Year(), date.Month(), date.Day(), 11, 0, 0, 0, time.UTC)
		rows = append(rows, pspRow{
			RecordID:   recordID,
			Type:       "refund",
			OccurredAt: refunded,
			GrossMinor: -amount,
			NetMinor:   -amount,
			Currency:   currency,
			Reference:  reference,
			BatchID:    batchID,
			Memo:       "synthetic refund",
		})
		lines := []Line{
			b.line("refunds_expense_"+lower, "debit", amount, currency, "", nil),
			b.line("clearing_"+SourcePSP+"_"+lower, "credit", amount, currency, reference,
				map[string]any{"source": SourcePSP, "source_record_id": recordID}),
		}
		b.addTxn("refund", "synthetic refund "+recordID, date, refunded,
			SourcePSP, recordID, -amount, lines)
	}

	pspFee := b.cfg.PSPFeeMinor[currency]
	if pspFee <= 0 {
		pspFee = 1
	}
	feeRecordID := fmt.Sprintf("psp_%s_%s_f01", compact, currency)
	feeReference := "REF-" + feeRecordID
	feeAt := time.Date(date.Year(), date.Month(), date.Day(), 12, 0, 0, 0, time.UTC)
	rows = append(rows, pspRow{
		RecordID:   feeRecordID,
		Type:       "fee",
		OccurredAt: feeAt,
		FeeMinor:   pspFee,
		NetMinor:   -pspFee,
		Currency:   currency,
		Reference:  feeReference,
		BatchID:    batchID,
		Memo:       "synthetic PSP fee",
	})
	b.addTxn("fee", "synthetic PSP fee "+feeRecordID, date, feeAt, SourcePSP, feeRecordID, -pspFee,
		[]Line{
			b.line("fees_expense_"+lower, "debit", pspFee, currency, "", nil),
			b.line("clearing_"+SourcePSP+"_"+lower, "credit", pspFee, currency, feeReference,
				map[string]any{"source": SourcePSP, "source_record_id": feeRecordID}),
		})

	if day == 2 {
		amount := b.chargeAmount(currency) / 2
		if amount < 1 {
			amount = 1
		}
		recordID := fmt.Sprintf("psp_%s_%s_b01", compact, currency)
		reference := "REF-" + recordID
		occurred := time.Date(date.Year(), date.Month(), date.Day(), 13, 0, 0, 0, time.UTC)
		rows = append(rows, pspRow{
			RecordID:   recordID,
			Type:       "chargeback",
			OccurredAt: occurred,
			GrossMinor: -amount,
			NetMinor:   -amount,
			Currency:   currency,
			Reference:  reference,
			BatchID:    batchID,
			Memo:       "synthetic chargeback",
		})
		b.addTxn("chargeback", "synthetic chargeback "+recordID, date, occurred,
			SourcePSP, recordID, -amount,
			[]Line{
				b.line("chargebacks_expense_"+lower, "debit", amount, currency, "", nil),
				b.line("clearing_"+SourcePSP+"_"+lower, "credit", amount, currency, reference,
					map[string]any{"source": SourcePSP, "source_record_id": recordID}),
			})
	}

	payout := chargeNetTotal
	if payout <= 0 {
		payout = 1
	}
	payoutRecordID := fmt.Sprintf("psp_%s_%s_p01", compact, currency)
	payoutReference := "REF-" + payoutRecordID
	payoutAt := time.Date(date.Year(), date.Month(), date.Day(), 14, 0, 0, 0, time.UTC)
	rows = append(rows, pspRow{
		RecordID:   payoutRecordID,
		Type:       "payout",
		OccurredAt: payoutAt,
		SettledAt:  &payoutAt,
		GrossMinor: -payout,
		NetMinor:   -payout,
		Currency:   currency,
		Reference:  payoutReference,
		BatchID:    batchID,
		Memo:       "synthetic settlement payout",
	})
	b.addTxn("payout", "synthetic payout "+payoutRecordID, date, payoutAt,
		SourcePSP, payoutRecordID, -payout,
		[]Line{
			b.line("payout_in_transit_"+lower, "debit", payout, currency, "", nil),
			b.line("clearing_"+SourcePSP+"_"+lower, "credit", payout, currency, payoutReference,
				map[string]any{"source": SourcePSP, "source_record_id": payoutRecordID}),
		})

	sort.Slice(rows, func(i, j int) bool { return rows[i].RecordID < rows[j].RecordID })
	b.pspBatches = append(b.pspBatches, pspBatch{
		Date:     date,
		Currency: currency,
		BatchID:  batchID,
		Rows:     rows,
	})

	bankFee := b.cfg.BankFeeMinor[currency]
	if bankFee <= 0 {
		bankFee = 1
	}
	opening := b.opening[currency]
	if opening == 0 {
		opening = b.cfg.OpeningBalanceMinor[currency]
	}
	closing := opening + payout - bankFee
	bankRows := []bankRow{
		{
			RecordID:      fmt.Sprintf("bnk_%s_%s_payout", compact, currency),
			BookingDate:   date,
			ValueDate:     date,
			AmountMinor:   payout,
			Currency:      currency,
			Description:   "payout from psp_a batch " + batchID,
			BankReference: "BNK-" + fmt.Sprintf("%s_%s_payout", compact, currency),
		},
		{
			RecordID:      fmt.Sprintf("bnk_%s_%s_fee", compact, currency),
			BookingDate:   date,
			ValueDate:     date,
			AmountMinor:   -bankFee,
			Currency:      currency,
			Description:   "synthetic account fee",
			BankReference: "BNK-" + fmt.Sprintf("%s_%s_fee", compact, currency),
		},
	}
	receipt := bankRows[0]
	feeRow := bankRows[1]
	sort.Slice(bankRows, func(i, j int) bool { return bankRows[i].RecordID < bankRows[j].RecordID })
	b.bankStatements = append(b.bankStatements, bankStatement{
		Date:         date,
		Currency:     currency,
		OpeningMinor: opening,
		ClosingMinor: closing,
		Rows:         bankRows,
	})
	b.opening[currency] = closing

	receiptAt := time.Date(date.Year(), date.Month(), date.Day(), 9, 0, 0, 0, time.UTC)
	b.addTxn("payout", "synthetic bank payout receipt "+receipt.RecordID, receipt.ValueDate, receiptAt,
		SourceBank, receipt.RecordID, receipt.AmountMinor,
		[]Line{
			b.line("clearing_"+SourceBank+"_"+lower, "debit", receipt.AmountMinor, currency, receipt.BankReference,
				map[string]any{"source": SourceBank, "source_record_id": receipt.RecordID}),
			b.line("payout_in_transit_"+lower, "credit", receipt.AmountMinor, currency, "", nil),
		})

	bankFeeAt := time.Date(date.Year(), date.Month(), date.Day(), 9, 30, 0, 0, time.UTC)
	b.addTxn("fee", "synthetic bank fee "+feeRow.RecordID, feeRow.ValueDate, bankFeeAt,
		SourceBank, feeRow.RecordID, feeRow.AmountMinor,
		[]Line{
			b.line("fees_expense_"+lower, "debit", bankFee, currency, "", nil),
			b.line("clearing_"+SourceBank+"_"+lower, "credit", bankFee, currency, feeRow.BankReference,
				map[string]any{"source": SourceBank, "source_record_id": feeRow.RecordID}),
		})

	return nil
}

func (b *builder) chargeAmount(currency string) int64 {
	switch currency {
	case "JPY":
		return 500 + b.rng.Int64N(9_500)
	case "SEK":
		return 10_000 + b.rng.Int64N(190_000)
	case "EUR":
		return 1_000 + b.rng.Int64N(19_000)
	default:
		return 1_000 + b.rng.Int64N(24_000)
	}
}

func (b *builder) refundAmount(currency string, exponent int) int64 {
	amount := b.chargeAmount(currency) / 2
	minimum := int64(1)
	if exponent > 0 {
		minimum = int64(100)
	}
	if amount < minimum {
		amount = minimum
	}
	return amount
}

func (b *builder) line(code, direction string, amount int64, currency, externalRef string, metadata map[string]any) Line {
	account := b.accountByCode[code]
	return Line{
		AccountID:   account.ID,
		AccountCode: code,
		Direction:   direction,
		AmountMinor: amount,
		Currency:    currency,
		ExternalRef: externalRef,
		Metadata:    metadata,
	}
}

// addTxn appends one ledger transaction and its clean ground-truth scenario.
// M1-09 maintains the one-to-one invariant: one external record per ledger
// transaction, so ledger txn count == external record count == link count.
func (b *builder) addTxn(kind, description string, valueDate, occurredAt time.Time,
	source, sourceRecordID string, externalAmount int64, lines []Line) {
	id := logicalID(b.opts.Seed, "txn", strconv.Itoa(b.nextTxn))
	b.nextTxn++
	b.externalCount++

	accountCodes := make([]string, 0, len(lines))
	seen := make(map[string]struct{}, len(lines))
	for _, line := range lines {
		if _, ok := seen[line.AccountCode]; ok {
			continue
		}
		seen[line.AccountCode] = struct{}{}
		accountCodes = append(accountCodes, line.AccountCode)
	}
	sort.Strings(accountCodes)

	scenario := Scenario{
		ScenarioID:          logicalID(b.opts.Seed, "scenario", source, sourceRecordID),
		Mode:                "clean",
		Difficulty:          b.opts.Difficulty,
		BreakSignature:      "",
		ExpectedResolutions: []string{"link"},
		PrimaryResolution:   "link",
		ExpectedTier:        "T0",
		Amounts:             []Amount{{Currency: lines[0].Currency, AmountMinor: externalAmount}},
		Accounts:            accountCodes,
		LedgerTxnIDs:        []string{id},
		ExternalRecords:     []RecordRef{{Source: source, SourceRecordID: sourceRecordID}},
	}
	b.scenarios = append(b.scenarios, scenario)

	b.transactions = append(b.transactions, Transaction{
		ID:             id,
		IdempotencyKey: "synth:" + id,
		Kind:           kind,
		ValueDate:      valueDate.Format(dateLayout),
		CreatedAt:      occurredAt.UTC().Format(timestampLayout),
		BookedAt:       occurredAt.UTC().Format(timestampLayout),
		Description:    description,
		Actor:          "system:synth",
		Lines:          lines,
	})
}

func (b *builder) finish() (*Dataset, error) {
	sort.Slice(b.scenarios, func(i, j int) bool {
		return b.scenarios[i].ScenarioID < b.scenarios[j].ScenarioID
	})
	sort.Slice(b.transactions, func(i, j int) bool {
		return b.transactions[i].ID < b.transactions[j].ID
	})

	manifest := Manifest{
		GeneratorVersion: GeneratorVersion,
		Seed:             b.opts.Seed,
		Days:             b.opts.Days,
		Difficulty:       b.opts.Difficulty,
		ConfigHash:       b.configHash,
		DatasetVersion:   b.dataset,
		Counts: Counts{
			Accounts:         len(b.accounts),
			Customers:        len(b.customers),
			Merchants:        len(b.merchants),
			FeeSchedules:     len(b.schedules),
			LedgerTxns:       len(b.transactions),
			ExternalRecords:  b.externalCount,
			GroundTruthLinks: b.externalCount,
		},
	}
	ledger := LedgerDocument{
		GeneratorVersion:     GeneratorVersion,
		DatasetVersion:       b.dataset,
		Accounts:             b.accounts,
		Customers:            b.customers,
		Merchants:            b.merchants,
		FeeSchedules:         b.schedules,
		ReconciliationScopes: b.scopes,
		Transactions:         b.transactions,
	}
	truth := GroundTruth{DatasetVersion: b.dataset, Scenarios: b.scenarios}

	files, err := render(b.pspBatches, b.bankStatements, manifest, ledger, truth)
	if err != nil {
		return nil, err
	}
	return &Dataset{Manifest: manifest, Ledger: ledger, Truth: truth, Files: files}, nil
}

// Write writes every rendered file under dir. Directories are created as
// needed; existing files are overwritten with identical or new bytes.
func (d *Dataset) Write(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return errors.New("synth: output directory is required")
	}
	for _, file := range d.Files {
		full := filepath.Join(dir, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("synth: create directory for %s: %w", file.Path, err)
		}
		if err := os.WriteFile(full, file.Data, 0o644); err != nil {
			return fmt.Errorf("synth: write %s: %w", file.Path, err)
		}
	}
	return nil
}
