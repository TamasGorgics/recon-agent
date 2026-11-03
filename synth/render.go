package synth

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strconv"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/money"
)

// pspRow is one rendered psp_a data row. Gross, Fee, and Net are integer minor
// units; the CSV renderer converts them to exact decimal strings.
type pspRow struct {
	RecordID   string
	Type       string
	OccurredAt time.Time
	SettledAt  *time.Time
	GrossMinor int64
	FeeMinor   int64
	NetMinor   int64
	Currency   string
	Reference  string
	BatchID    string
	Memo       string
}

type pspBatch struct {
	Date     time.Time
	Currency string
	BatchID  string
	Rows     []pspRow
}

type bankRow struct {
	RecordID      string
	BookingDate   time.Time
	ValueDate     time.Time
	AmountMinor   int64
	Currency      string
	Description   string
	BankReference string
}

type bankStatement struct {
	Date         time.Time
	Currency     string
	OpeningMinor int64
	ClosingMinor int64
	Rows         []bankRow
}

var (
	pspHeader  = "record_id,type,occurred_at,settled_at,gross,fee,net,currency,reference,batch_id,memo"
	bankHeader = "record_id,booking_date,value_date,amount,currency,description,bank_reference"
)

func render(pspBatches []pspBatch, statements []bankStatement, manifest Manifest,
	ledger LedgerDocument, truth GroundTruth) ([]File, error) {
	var files []File
	for _, name := range []string{"manifest.json", "ledger.json", "ground_truth.json"} {
		var payload any
		switch name {
		case "manifest.json":
			payload = manifest
		case "ledger.json":
			payload = ledger
		default:
			payload = truth
		}
		data, err := jsonDocument(payload)
		if err != nil {
			return nil, fmt.Errorf("synth: render %s: %w", name, err)
		}
		files = append(files, File{Path: name, Data: data})
	}

	for _, batch := range pspBatches {
		data, err := renderPSP(batch)
		if err != nil {
			return nil, err
		}
		files = append(files, File{
			Path: path.Join(SourcePSP, batch.BatchID+".csv"),
			Data: data,
		})
	}
	for _, statement := range statements {
		data, err := renderBank(statement)
		if err != nil {
			return nil, err
		}
		files = append(files, File{
			Path: path.Join(SourceBank, statement.Date.Format(dateLayout)+"_"+statement.Currency+".csv"),
			Data: data,
		})
	}

	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	for i := 1; i < len(files); i++ {
		if files[i].Path == files[i-1].Path {
			return nil, fmt.Errorf("synth: duplicate output path %q", files[i].Path)
		}
	}
	return files, nil
}

func renderPSP(batch pspBatch) ([]byte, error) {
	rows := append([]pspRow(nil), batch.Rows...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].RecordID < rows[j].RecordID })
	var total int64
	for _, row := range rows {
		total += row.NetMinor
	}
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "# rows=%d total=%s currency=%s batch=%s period_start=%s period_end=%s\n",
		len(rows), formatMinor(total, batch.Currency), batch.Currency, batch.BatchID,
		batch.Date.Format(dateLayout), batch.Date.Format(dateLayout))
	if _, err := buf.WriteString(pspHeader + "\n"); err != nil {
		return nil, err
	}
	writer := csv.NewWriter(&buf)
	for _, row := range rows {
		settled := ""
		if row.SettledAt != nil {
			settled = row.SettledAt.UTC().Format(timestampLayout)
		}
		if err := writer.Write([]string{
			row.RecordID,
			row.Type,
			row.OccurredAt.UTC().Format(timestampLayout),
			settled,
			formatMinor(row.GrossMinor, row.Currency),
			formatMinor(row.FeeMinor, row.Currency),
			formatMinor(row.NetMinor, row.Currency),
			row.Currency,
			row.Reference,
			row.BatchID,
			row.Memo,
		}); err != nil {
			return nil, fmt.Errorf("synth: render psp batch %s: %w", batch.BatchID, err)
		}
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return nil, fmt.Errorf("synth: render psp batch %s: %w", batch.BatchID, err)
	}
	return buf.Bytes(), nil
}

func renderBank(statement bankStatement) ([]byte, error) {
	rows := append([]bankRow(nil), statement.Rows...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].RecordID < rows[j].RecordID })
	var total int64
	for _, row := range rows {
		total += row.AmountMinor
	}
	var buf bytes.Buffer
	fmt.Fprintf(&buf,
		"# rows=%d total=%s currency=%s opening=%s closing=%s as_of=%s\n",
		len(rows), formatMinor(total, statement.Currency), statement.Currency,
		formatMinor(statement.OpeningMinor, statement.Currency),
		formatMinor(statement.ClosingMinor, statement.Currency),
		statement.Date.Format(dateLayout))
	if _, err := buf.WriteString(bankHeader + "\n"); err != nil {
		return nil, err
	}
	writer := csv.NewWriter(&buf)
	for _, row := range rows {
		if err := writer.Write([]string{
			row.RecordID,
			row.BookingDate.Format(dateLayout),
			row.ValueDate.Format(dateLayout),
			formatMinor(row.AmountMinor, row.Currency),
			row.Currency,
			row.Description,
			row.BankReference,
		}); err != nil {
			return nil, fmt.Errorf("synth: render bank statement %s %s: %w",
				statement.Date.Format(dateLayout), statement.Currency, err)
		}
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return nil, fmt.Errorf("synth: render bank statement: %w", err)
	}
	return buf.Bytes(), nil
}

func jsonDocument(value any) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// formatMinor renders exact minor units as the decimal string the parsers
// accept. It never uses floating point (FIN-05).
func formatMinor(amount int64, currency string) string {
	parsed, err := money.ParseCurrency(currency)
	if err != nil {
		// Currencies are validated before generation; this is a programming
		// error, and rendering must not silently produce a wrong amount.
		panic("synth: invalid currency " + strconv.Quote(currency) + ": " + err.Error())
	}
	return money.FromMinor(amount, parsed).DecimalString()
}
