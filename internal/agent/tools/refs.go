package tools

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/TamasGorgics/recon-agent/internal/money"
)

var (
	uuidPattern   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	sourcePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	controlChars  = regexp.MustCompile(`[\x00-\x1f\x7f]`)
)

// LedgerTxnRef returns ledger_txn:<uuid>.
func LedgerTxnRef(txnID string) (string, error) {
	if !uuidPattern.MatchString(txnID) {
		return "", fmt.Errorf("%w: ledger txn id %q is not a UUID", ErrInvalidRef, txnID)
	}
	return "ledger_txn:" + txnID, nil
}

// ExternalRef returns ext:<source>:<source_record_id>. The record ID may
// contain colons; validation splits only after the source token.
func ExternalRef(source, recordID string) (string, error) {
	if !sourcePattern.MatchString(source) {
		return "", fmt.Errorf("%w: source %q is not a configured-source-shaped token", ErrInvalidRef, source)
	}
	if recordID == "" {
		return "", fmt.Errorf("%w: external record id is empty", ErrInvalidRef)
	}
	if controlChars.MatchString(recordID) {
		return "", fmt.Errorf("%w: external record id contains control characters", ErrInvalidRef)
	}
	return "ext:" + source + ":" + recordID, nil
}

// BatchRef returns batch:<uuid>.
func BatchRef(batchID string) (string, error) {
	if !uuidPattern.MatchString(batchID) {
		return "", fmt.Errorf("%w: batch id %q is not a UUID", ErrInvalidRef, batchID)
	}
	return "batch:" + batchID, nil
}

// FXRef returns fx:<BASE>/<QUOTE>@<YYYY-MM-DD> for an upper-case ISO pair with
// distinct currencies and a valid UTC date.
func FXRef(base, quote, date string) (string, error) {
	if _, err := money.ParseCurrency(base); err != nil {
		return "", fmt.Errorf("%w: base %q is not an ISO currency", ErrInvalidRef, base)
	}
	if _, err := money.ParseCurrency(quote); err != nil {
		return "", fmt.Errorf("%w: quote %q is not an ISO currency", ErrInvalidRef, quote)
	}
	if base == quote {
		return "", fmt.Errorf("%w: base and quote must differ", ErrInvalidRef)
	}
	if _, err := ParseDate(date); err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidRef, err)
	}
	return "fx:" + base + "/" + quote + "@" + date, nil
}

// FeeScheduleRef returns fee_schedule:<uuid>.
func FeeScheduleRef(scheduleID string) (string, error) {
	if !uuidPattern.MatchString(scheduleID) {
		return "", fmt.Errorf("%w: fee schedule id %q is not a UUID", ErrInvalidRef, scheduleID)
	}
	return "fee_schedule:" + scheduleID, nil
}

// CaseRef returns case:<uuid>.
func CaseRef(caseID string) (string, error) {
	if !uuidPattern.MatchString(caseID) {
		return "", fmt.Errorf("%w: case id %q is not a UUID", ErrInvalidRef, caseID)
	}
	return "case:" + caseID, nil
}

// ValidateRef checks an evidence reference against the six supported formats.
func ValidateRef(ref string) error {
	switch {
	case strings.HasPrefix(ref, "ledger_txn:"):
		return validateUUIDRef(ref, "ledger_txn:")
	case strings.HasPrefix(ref, "batch:"):
		return validateUUIDRef(ref, "batch:")
	case strings.HasPrefix(ref, "fee_schedule:"):
		return validateUUIDRef(ref, "fee_schedule:")
	case strings.HasPrefix(ref, "case:"):
		return validateUUIDRef(ref, "case:")
	case strings.HasPrefix(ref, "ext:"):
		rest := strings.TrimPrefix(ref, "ext:")
		source, recordID, ok := strings.Cut(rest, ":")
		if !ok {
			return fmt.Errorf("%w: external reference %q has no record id", ErrInvalidRef, ref)
		}
		want, err := ExternalRef(source, recordID)
		if err != nil {
			return err
		}
		if want != ref {
			return fmt.Errorf("%w: external reference %q is not canonical", ErrInvalidRef, ref)
		}
		return nil
	case strings.HasPrefix(ref, "fx:"):
		rest := strings.TrimPrefix(ref, "fx:")
		pair, date, ok := strings.Cut(rest, "@")
		if !ok {
			return fmt.Errorf("%w: fx reference %q has no date", ErrInvalidRef, ref)
		}
		base, quote, ok := strings.Cut(pair, "/")
		if !ok {
			return fmt.Errorf("%w: fx reference %q has no quote currency", ErrInvalidRef, ref)
		}
		want, err := FXRef(base, quote, date)
		if err != nil {
			return err
		}
		if want != ref {
			return fmt.Errorf("%w: fx reference %q is not canonical", ErrInvalidRef, ref)
		}
		return nil
	default:
		return fmt.Errorf("%w: %q has an unknown prefix", ErrInvalidRef, ref)
	}
}

func validateUUIDRef(ref, prefix string) error {
	id := strings.TrimPrefix(ref, prefix)
	if !uuidPattern.MatchString(id) {
		return fmt.Errorf("%w: %q is not a canonical %s<uuid> reference", ErrInvalidRef, ref, prefix)
	}
	if prefix+id != ref {
		return fmt.Errorf("%w: %q is not canonical", ErrInvalidRef, ref)
	}
	return nil
}
