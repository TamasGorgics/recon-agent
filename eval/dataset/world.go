package dataset

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"sort"
	"strings"

	"github.com/TamasGorgics/recon-agent/internal/money"
	"github.com/TamasGorgics/recon-agent/synth"
	"github.com/TamasGorgics/recon-agent/synth/modes"
)

var pspColumns = []string{
	"record_id",
	"type",
	"occurred_at",
	"settled_at",
	"gross",
	"fee",
	"net",
	"currency",
	"reference",
	"batch_id",
	"memo",
}

var bankColumns = []string{
	"record_id",
	"booking_date",
	"value_date",
	"amount",
	"currency",
	"description",
	"bank_reference",
}

// recordRef is the lookup key for one generated external record.
type recordRef struct {
	Source         string
	SourceRecordID string
}

func refOf(record synth.RecordRef) recordRef {
	return recordRef{Source: record.Source, SourceRecordID: record.SourceRecordID}
}

// externalRecord is the parsed subset of a psp_a/bank_x row the eval builder
// needs for truth derivation and reality checks.
type externalRecord struct {
	Source     string
	RecordID   string
	Currency   string
	Reference  string
	Memo       string
	GrossMinor int64
	NetMinor   int64
}

// evalWorld indexes the generated files and ledger so ground truth can be
// derived from data rather than guessed.
type evalWorld struct {
	external map[recordRef]externalRecord
	txns     map[string]synth.Transaction
	accounts map[string]synth.Account
}

func newEvalWorld(result *modes.Result) (*evalWorld, error) {
	if result == nil {
		return nil, fmt.Errorf("eval/dataset: modes result is required")
	}
	world := &evalWorld{
		external: make(map[recordRef]externalRecord),
		txns:     make(map[string]synth.Transaction, len(result.Ledger.Transactions)),
		accounts: make(map[string]synth.Account, len(result.Ledger.Accounts)),
	}
	for _, account := range result.Ledger.Accounts {
		if account.Code == "" {
			return nil, fmt.Errorf("eval/dataset: generated account has an empty code")
		}
		if _, duplicate := world.accounts[account.Code]; duplicate {
			return nil, fmt.Errorf("eval/dataset: duplicate generated account %q", account.Code)
		}
		world.accounts[account.Code] = account
	}
	for _, txn := range result.Ledger.Transactions {
		if txn.ID == "" {
			return nil, fmt.Errorf("eval/dataset: generated transaction has an empty id")
		}
		if _, duplicate := world.txns[txn.ID]; duplicate {
			return nil, fmt.Errorf("eval/dataset: duplicate generated transaction %q", txn.ID)
		}
		world.txns[txn.ID] = txn
	}
	for _, file := range result.Files {
		switch {
		case strings.HasPrefix(file.Path, synth.SourcePSP+"/"):
			records, err := parseExternalFile(file, synth.SourcePSP, pspColumns)
			if err != nil {
				return nil, err
			}
			if err := world.addExternal(records); err != nil {
				return nil, err
			}
		case strings.HasPrefix(file.Path, synth.SourceBank+"/"):
			records, err := parseExternalFile(file, synth.SourceBank, bankColumns)
			if err != nil {
				return nil, err
			}
			if err := world.addExternal(records); err != nil {
				return nil, err
			}
		}
	}
	return world, nil
}

func (w *evalWorld) addExternal(records []externalRecord) error {
	for _, record := range records {
		key := recordRef{Source: record.Source, SourceRecordID: record.RecordID}
		if _, duplicate := w.external[key]; duplicate {
			return fmt.Errorf("eval/dataset: duplicate external record %s:%s",
				record.Source, record.RecordID)
		}
		w.external[key] = record
	}
	return nil
}

func (w *evalWorld) lookup(record synth.RecordRef) (externalRecord, bool) {
	value, ok := w.external[refOf(record)]
	return value, ok
}

// parseExternalFile parses one generated psp_a or bank_x CSV. The column set is
// fixed by docs/formats.md and the generator renders exact headers.
func parseExternalFile(file synth.File, source string, columns []string) ([]externalRecord, error) {
	newline := bytes.IndexByte(file.Data, '\n')
	if newline < 0 {
		return nil, fmt.Errorf("eval/dataset: %s has no metadata line", file.Path)
	}
	metadata := make(map[string]string)
	for _, token := range strings.Fields(strings.TrimSpace(string(file.Data[:newline]))) {
		if !strings.HasPrefix(token, "#") {
			key, value, ok := strings.Cut(token, "=")
			if !ok || key == "" || value == "" {
				return nil, fmt.Errorf("eval/dataset: %s metadata token %q is malformed",
					file.Path, token)
			}
			metadata[key] = value
		}
	}
	currencyCode := metadata["currency"]
	if currencyCode == "" {
		return nil, fmt.Errorf("eval/dataset: %s has no currency metadata", file.Path)
	}
	currency, err := money.ParseCurrency(currencyCode)
	if err != nil {
		return nil, fmt.Errorf("eval/dataset: %s currency: %w", file.Path, err)
	}
	reader := csv.NewReader(bytes.NewReader(file.Data[newline+1:]))
	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("eval/dataset: %s: %w", file.Path, err)
	}
	if len(records) == 0 || strings.Join(records[0], ",") != strings.Join(columns, ",") {
		return nil, fmt.Errorf("eval/dataset: %s header does not match %s", file.Path, source)
	}
	parsed := make([]externalRecord, 0, len(records)-1)
	for index, fields := range records[1:] {
		if len(fields) != len(columns) {
			return nil, fmt.Errorf("eval/dataset: %s row %d has %d fields",
				file.Path, index+2, len(fields))
		}
		record := externalRecord{
			Source:   source,
			RecordID: fields[0],
			Currency: currencyCode,
		}
		if source == synth.SourceBank {
			record.Reference = fields[6]
			record.Memo = fields[5]
			amount, err := money.ParseDecimal(fields[3], currency)
			if err != nil {
				return nil, fmt.Errorf("eval/dataset: %s row %d amount: %w",
					file.Path, index+2, err)
			}
			record.GrossMinor = amount.AmountMinor()
			record.NetMinor = amount.AmountMinor()
		} else {
			record.Reference = fields[8]
			record.Memo = fields[10]
			gross, err := money.ParseDecimal(fields[4], currency)
			if err != nil {
				return nil, fmt.Errorf("eval/dataset: %s row %d gross: %w",
					file.Path, index+2, err)
			}
			fee, err := money.ParseDecimal(fields[5], currency)
			if err != nil {
				return nil, fmt.Errorf("eval/dataset: %s row %d fee: %w",
					file.Path, index+2, err)
			}
			net, err := money.ParseDecimal(fields[6], currency)
			if err != nil {
				return nil, fmt.Errorf("eval/dataset: %s row %d net: %w",
					file.Path, index+2, err)
			}
			record.GrossMinor = gross.AmountMinor()
			record.NetMinor = net.AmountMinor()
			if record.GrossMinor-record.NetMinor != fee.AmountMinor() {
				return nil, fmt.Errorf("eval/dataset: %s row %d gross-fee-net is not zero",
					file.Path, index+2)
			}
		}
		if record.RecordID == "" {
			return nil, fmt.Errorf("eval/dataset: %s row %d has an empty record_id",
				file.Path, index+2)
		}
		parsed = append(parsed, record)
	}
	return parsed, nil
}

func txnCurrency(txn synth.Transaction) (string, error) {
	if len(txn.Lines) == 0 {
		return "", fmt.Errorf("eval/dataset: transaction %s has no lines", txn.ID)
	}
	return txn.Lines[0].Currency, nil
}

func txnHasAccount(txn synth.Transaction, code string) bool {
	for _, line := range txn.Lines {
		if line.AccountCode == code {
			return true
		}
	}
	return false
}

func txnHasExternalRef(txn synth.Transaction, reference string) bool {
	for _, line := range txn.Lines {
		if line.ExternalRef == reference {
			return true
		}
	}
	return false
}

// sortedRecordRefs returns a copy sorted by (source, record id). The generated
// scenario order is deterministic already; the eval document pins the canonical
// order required by the truth contract.
func sortedRecordRefs(records []synth.RecordRef) []synth.RecordRef {
	copied := make([]synth.RecordRef, len(records))
	copy(copied, records)
	sort.Slice(copied, func(i, j int) bool {
		if copied[i].Source != copied[j].Source {
			return copied[i].Source < copied[j].Source
		}
		return copied[i].SourceRecordID < copied[j].SourceRecordID
	})
	return copied
}

// requireSortedUniqueRefs checks a sorted record-ref slice has no duplicate.
func requireSortedUniqueRefs(where string, records []synth.RecordRef) error {
	for index := 1; index < len(records); index++ {
		previous, current := records[index-1], records[index]
		if previous == current {
			return fmt.Errorf("%s has duplicate record %s:%s",
				where, current.Source, current.SourceRecordID)
		}
	}
	return nil
}
