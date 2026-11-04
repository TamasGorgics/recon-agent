package ingest

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/TamasGorgics/recon-agent/internal/money"
)

// dateLayout is the only accepted date representation in both formats.
const dateLayout = "2006-01-02"

// maxAmountBytes bounds an amount token before it is handed to
// money.ParseDecimal. An int64 minor-unit value has at most 19 digits, so a
// sign, decimal point, and three fractional digits fit easily; the bound keeps
// pathological multi-megabyte digit strings from dominating parse time while
// still accepting every representable amount.
const maxAmountBytes = 32

// utf8BOM is the optional leading byte-order mark accepted on every file.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// sourceFile is the parsed common envelope: metadata key/value pairs and the
// data rows with the header already removed and the column count validated.
type sourceFile struct {
	metadata map[string]string
	rows     [][]string
}

// readSourceFile parses the common psp_a/bank_x envelope:
//
//	[BOM] metadata-line (\n | \r\n)
//	header-line
//	data rows (RFC 4180 CSV)
//
// It rejects invalid UTF-8, a missing or malformed metadata line, a wrong
// header, and rows with the wrong field count. Values are not interpreted here;
// the format-specific parser validates and converts them.
func readSourceFile(r io.Reader, columns []string) (*sourceFile, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("%w: read file: %v", ErrInvalidFormat, err)
	}
	data = bytes.TrimPrefix(data, utf8BOM)
	if !utf8.Valid(data) {
		return nil, ErrInvalidEncoding
	}

	metadataLine, rest, err := splitMetadataLine(data)
	if err != nil {
		return nil, err
	}
	metadata, err := parseMetadataLine(metadataLine)
	if err != nil {
		return nil, err
	}

	reader := csv.NewReader(bytes.NewReader(rest))
	reader.FieldsPerRecord = -1
	reader.ReuseRecord = false
	reader.TrimLeadingSpace = false
	reader.LazyQuotes = false

	header, err := reader.Read()
	if errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: file has no header line", ErrInvalidFormat)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: read header: %v", ErrMalformedRow, err)
	}
	if err := checkColumns(header, columns); err != nil {
		return nil, err
	}

	rows := make([][]string, 0, 16)
	for {
		row, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: data row %d: %v", ErrMalformedRow, len(rows)+1, err)
		}
		if len(row) != len(columns) {
			return nil, fmt.Errorf("%w: data row %d has %d fields, want %d",
				ErrMalformedRow, len(rows)+1, len(row), len(columns))
		}
		rows = append(rows, row)
	}
	return &sourceFile{metadata: metadata, rows: rows}, nil
}

// splitMetadataLine returns the first non-blank line (which must start with #)
// and the bytes after it. Blank lines before the metadata are ignored.
func splitMetadataLine(data []byte) (string, []byte, error) {
	offset := 0
	for {
		if offset >= len(data) {
			return "", nil, fmt.Errorf("%w: file is empty or has no metadata line", ErrBadMetadata)
		}
		end := bytes.IndexByte(data[offset:], '\n')
		next := len(data)
		var line []byte
		if end >= 0 {
			line = data[offset : offset+end]
			next = offset + end + 1
		} else {
			line = data[offset:]
		}
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(bytes.TrimSpace(line)) == 0 {
			offset = next
			continue
		}
		if line[0] != '#' {
			return "", nil, fmt.Errorf("%w: first non-blank line must be metadata starting with #", ErrBadMetadata)
		}
		return string(line), data[next:], nil
	}
}

// parseMetadataLine splits "# key=value key=value" into a map. Keys are
// lower-case ASCII; values are non-empty tokens without spaces.
func parseMetadataLine(line string) (map[string]string, error) {
	tokens := strings.Fields(strings.TrimPrefix(line, "#"))
	if len(tokens) == 0 {
		return nil, fmt.Errorf("%w: metadata line has no key=value pairs", ErrBadMetadata)
	}
	values := make(map[string]string, len(tokens))
	for _, token := range tokens {
		key, value, ok := strings.Cut(token, "=")
		if !ok {
			return nil, fmt.Errorf("%w: token %q is not key=value", ErrBadMetadata, token)
		}
		if !validMetadataKey(key) {
			return nil, fmt.Errorf("%w: invalid key %q", ErrBadMetadata, key)
		}
		if value == "" {
			return nil, fmt.Errorf("%w: key %q has an empty value", ErrBadMetadata, key)
		}
		if _, duplicate := values[key]; duplicate {
			return nil, fmt.Errorf("%w: duplicate key %q", ErrBadMetadata, key)
		}
		values[key] = value
	}
	return values, nil
}

// validMetadataKey reports whether key is [a-z][a-z0-9_]*.
func validMetadataKey(key string) bool {
	if key == "" {
		return false
	}
	for index := 0; index < len(key); index++ {
		char := key[index]
		switch {
		case char >= 'a' && char <= 'z':
		case index > 0 && (char >= '0' && char <= '9' || char == '_'):
		default:
			return false
		}
	}
	return true
}

// checkMetadata rejects unknown keys and reports the first missing required
// key. Both are ErrBadMetadata: the file cannot be trusted to describe itself.
func checkMetadata(values map[string]string, required, optional []string) error {
	allowed := make(map[string]struct{}, len(required)+len(optional))
	for _, key := range required {
		allowed[key] = struct{}{}
	}
	for _, key := range optional {
		allowed[key] = struct{}{}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, ok := allowed[key]; !ok {
			return fmt.Errorf("%w: unknown key %q", ErrBadMetadata, key)
		}
	}
	for _, key := range required {
		if _, ok := values[key]; !ok {
			return fmt.Errorf("%w: missing key %q", ErrBadMetadata, key)
		}
	}
	return nil
}

// checkColumns requires the header to match the documented names exactly.
func checkColumns(header, want []string) error {
	present := make(map[string]struct{}, len(header))
	for _, name := range header {
		present[name] = struct{}{}
	}
	if len(header) < len(want) {
		var missing []string
		for _, name := range want {
			if _, ok := present[name]; !ok {
				missing = append(missing, name)
			}
		}
		if len(missing) == 0 {
			missing = want[len(header):]
		}
		return fmt.Errorf("%w: missing %s", ErrMissingColumn, strings.Join(missing, ", "))
	}
	if len(header) > len(want) {
		var extra []string
		wanted := make(map[string]struct{}, len(want))
		for _, name := range want {
			wanted[name] = struct{}{}
		}
		for _, name := range header {
			if _, ok := wanted[name]; !ok {
				extra = append(extra, name)
			}
		}
		if len(extra) == 0 {
			extra = header[len(want):]
		}
		return fmt.Errorf("%w: unexpected %s", ErrUnknownColumn, strings.Join(extra, ", "))
	}
	for index, name := range want {
		if header[index] != name {
			return fmt.Errorf("%w: column %d is %q, want %q", ErrUnknownColumn, index+1, header[index], name)
		}
	}
	return nil
}

// intValue parses a non-negative integer metadata value.
func (f *sourceFile) intValue(key string) (int, error) {
	raw := f.metadata[key]
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%w: key %q value %q is not an integer", ErrBadMetadata, key, raw)
	}
	if value < 0 {
		return 0, fmt.Errorf("%w: key %q value %d is negative", ErrBadMetadata, key, value)
	}
	return value, nil
}

// amountValue parses an exact decimal metadata amount in currency.
func (f *sourceFile) amountValue(key string, currency money.Currency) (money.Money, error) {
	raw := f.metadata[key]
	if len(raw) > maxAmountBytes {
		return money.Money{}, fmt.Errorf("%w: key %q value is longer than %d bytes",
			ErrBadMetadata, key, maxAmountBytes)
	}
	amount, err := money.ParseDecimal(raw, currency)
	if err != nil {
		return money.Money{}, fmt.Errorf("%w: key %q value %q: %w", ErrBadMetadata, key, raw, err)
	}
	return amount, nil
}

// dateValue parses a YYYY-MM-DD metadata value. The zero time (year 1) is
// rejected because it is indistinguishable from "missing".
func (f *sourceFile) dateValue(key string) (time.Time, error) {
	raw := f.metadata[key]
	parsed, err := time.Parse(dateLayout, raw)
	if err != nil || parsed.IsZero() {
		return time.Time{}, fmt.Errorf("%w: key %q value %q", ErrMalformedDate, key, raw)
	}
	return parsed, nil
}

// rowAmount parses an exact decimal row amount in currency.
func rowAmount(value, column string, currency money.Currency) (money.Money, error) {
	if len(value) > maxAmountBytes {
		return money.Money{}, fmt.Errorf("%w: column %q value is longer than %d bytes",
			ErrInvalidAmount, column, maxAmountBytes)
	}
	amount, err := money.ParseDecimal(value, currency)
	if err != nil {
		return money.Money{}, fmt.Errorf("%w: column %q value %q: %w", ErrInvalidAmount, column, value, err)
	}
	return amount, nil
}

// rowDate parses a YYYY-MM-DD row value. The zero time (year 1) is rejected
// because it is indistinguishable from "missing".
func rowDate(value, column string) (time.Time, error) {
	parsed, err := time.Parse(dateLayout, value)
	if err != nil || parsed.IsZero() {
		return time.Time{}, fmt.Errorf("%w: column %q value %q", ErrMalformedDate, column, value)
	}
	return parsed, nil
}

// rowTimestamp parses an RFC 3339 row value and normalizes it to UTC. The zero
// time is rejected: it cannot be distinguished from a missing timestamp.
func rowTimestamp(value, column string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil || parsed.IsZero() {
		return time.Time{}, fmt.Errorf("%w: column %q value %q is not RFC 3339", ErrMalformedDate, column, value)
	}
	return parsed.UTC(), nil
}

// declaredCurrency parses the metadata currency once for the whole file.
func (f *sourceFile) declaredCurrency() (money.Currency, error) {
	code := f.metadata["currency"]
	currency, err := money.ParseCurrency(code)
	if err != nil {
		return money.Currency{}, fmt.Errorf("%w: key %q value %q: %w", ErrBadMetadata, "currency", code, err)
	}
	return currency, nil
}
