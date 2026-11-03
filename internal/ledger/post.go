package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/money"
	"github.com/TamasGorgics/recon-agent/internal/periods"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func isUUID(value string) bool { return uuidPattern.MatchString(value) }

// Post validates req, resolves the posting period, inserts the transaction and
// its entries, and appends the audit row - all inside the caller's transaction
// (FIN-10, FIN-12, FIN-30, FIN-56).
//
// The idempotency key is checked first, so a replay returns the original
// transaction even if the payload would no longer validate today. Concurrent
// posts with the same key serialize on the unique index; the loser reads the
// committed winner and either replays it or reports ErrIdempotencyConflict.
func (s *Service) Post(ctx context.Context, tx pgx.Tx, req PostRequest) (Result, error) {
	if tx == nil {
		return Result{}, ErrNoTransaction
	}
	if s == nil || s.Clock == nil {
		return Result{}, ErrNoClock
	}

	hash, err := requestHash(req)
	if err != nil {
		return Result{}, err
	}

	// Idempotency fast path. Committed replays return the original before any
	// account, period, or balance validation runs.
	stored, err := lookupTxnByKey(ctx, tx, req.IdempotencyKey)
	switch {
	case err == nil:
		return replayStored(stored, hash)
	case !errors.Is(err, pgx.ErrNoRows):
		return Result{}, fmt.Errorf("ledger: look up idempotency key: %w", err)
	}

	if err := validatePostRequest(req); err != nil {
		return Result{}, err
	}
	accounts, err := loadAccounts(ctx, tx, req.Lines)
	if err != nil {
		return Result{}, err
	}
	if err := s.checkAccounts(req.Kind, req.Lines, accounts); err != nil {
		return Result{}, err
	}

	now := s.Clock.Now().UTC()
	createdAt := req.CreatedAt
	if createdAt.IsZero() {
		createdAt = now
	}
	bookedAt := req.BookedAt
	if bookedAt.IsZero() {
		bookedAt = createdAt
	}
	valueDate := dateOnlyValue(req.ValueDate)

	selection, err := periods.Resolve(ctx, tx, valueDate, now)
	if err != nil {
		return Result{}, err
	}

	if req.ReversalOf != "" {
		alreadyReversed, err := hasReversal(ctx, tx, req.ReversalOf)
		if err != nil {
			return Result{}, err
		}
		if alreadyReversed {
			return Result{}, fmt.Errorf("%w: %s", ErrAlreadyReversed, req.ReversalOf)
		}
	}

	var (
		id  string
		seq int64
	)
	err = tx.QueryRow(ctx,
		`INSERT INTO ledger_txns
		     (id, created_at, booked_at, value_date, kind, idempotency_key,
		      request_hash, description, period_id, adjusts_period_id, reversal_of)
		 VALUES
		     (COALESCE($1::uuid, gen_random_uuid()), $2, $3, $4::date, $5, $6,
		      $7, $8, $9::uuid, $10::uuid, $11::uuid)
		 ON CONFLICT (idempotency_key) DO NOTHING
		 RETURNING id::text, seq`,
		nullableString(req.ID), createdAt, bookedAt, valueDate.Format(dateLayout), string(req.Kind),
		req.IdempotencyKey, hash, req.Description, selection.PeriodID,
		nullableStringPtr(selection.AdjustsPeriodID), nullableString(req.ReversalOf)).
		Scan(&id, &seq)
	if errors.Is(err, pgx.ErrNoRows) {
		// A concurrent transaction with the same key won the insert race. Its
		// row is committed by the time ON CONFLICT returns, so a fresh
		// statement in READ COMMITTED sees it.
		winner, lookupErr := lookupTxnByKey(ctx, tx, req.IdempotencyKey)
		if lookupErr != nil {
			return Result{}, fmt.Errorf("ledger: resolve idempotency race: %w", lookupErr)
		}
		return replayStored(winner, hash)
	}
	if err != nil {
		return Result{}, fmt.Errorf("ledger: insert transaction: %w", err)
	}

	for i, line := range req.Lines {
		metadata, err := entryMetadata(line.Metadata)
		if err != nil {
			return Result{}, fmt.Errorf("%w: line %d metadata: %w", ErrInvalidLine, i, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO ledger_entries
			     (txn_id, account_id, direction, amount_minor, currency, posted_at, external_ref, metadata)
			 VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8::jsonb)`,
			id, line.AccountID, string(line.Direction), line.AmountMinor,
			line.Currency, bookedAt, nullableString(line.ExternalRef), metadata); err != nil {
			return Result{}, fmt.Errorf("ledger: insert entry %d: %w", i, err)
		}
	}

	result := Result{
		ID:              id,
		Seq:             seq,
		Kind:            req.Kind,
		ValueDate:       valueDate,
		CreatedAt:       createdAt,
		BookedAt:        bookedAt,
		PeriodID:        selection.PeriodID,
		AdjustsPeriodID: selection.AdjustsPeriodID,
		ReversalOf:      stringPtrOrNil(req.ReversalOf),
		IdempotencyKey:  req.IdempotencyKey,
		RequestHash:     hash,
	}
	if _, err := audit.Append(ctx, tx, audit.Entry{
		Actor:      req.Actor,
		Action:     actionFor(req.Kind),
		EntityType: EntityTxn,
		EntityID:   id,
		After:      auditPayload(result, req.Lines),
		Timestamp:  bookedAt,
	}); err != nil {
		return Result{}, fmt.Errorf("ledger: append audit entry: %w", err)
	}
	return result, nil
}

func actionFor(kind Kind) string {
	if kind == KindReversal {
		return ActionReverse
	}
	return ActionPost
}

// validatePostRequest performs every check the service owns before touching the
// database, except the account policy (which needs account codes).
func validatePostRequest(req PostRequest) error {
	if strings.TrimSpace(req.IdempotencyKey) == "" {
		return fmt.Errorf("%w: idempotency key is empty", ErrInvalidRequest)
	}
	if !req.Kind.valid() {
		return fmt.Errorf("%w: unknown kind %q", ErrInvalidRequest, req.Kind)
	}
	if req.Kind == KindReversal && req.ReversalOf == "" {
		return fmt.Errorf("%w: reversal has no reversal_of", ErrInvalidRequest)
	}
	if req.Kind != KindReversal && req.ReversalOf != "" {
		return fmt.Errorf("%w: reversal_of is only valid for kind=reversal", ErrInvalidRequest)
	}
	if req.ReversalOf != "" && !isUUID(req.ReversalOf) {
		return fmt.Errorf("%w: reversal_of %q is not a UUID", ErrInvalidRequest, req.ReversalOf)
	}
	if req.ValueDate.IsZero() {
		return fmt.Errorf("%w: value_date is required", ErrInvalidRequest)
	}
	if !req.Actor.Valid() {
		return fmt.Errorf("%w: actor is required", ErrInvalidRequest)
	}
	if len(req.Lines) == 0 {
		return ErrEmptyTransaction
	}
	for i, line := range req.Lines {
		if !isUUID(line.AccountID) {
			return fmt.Errorf("%w: line %d account id %q is not a UUID", ErrInvalidLine, i, line.AccountID)
		}
		if !line.Direction.valid() {
			return fmt.Errorf("%w: line %d direction %q", ErrInvalidLine, i, line.Direction)
		}
		if line.AmountMinor <= 0 {
			return fmt.Errorf("%w: line %d amount %d must be positive", ErrInvalidLine, i, line.AmountMinor)
		}
		if _, err := money.ParseCurrency(line.Currency); err != nil {
			return fmt.Errorf("%w: line %d currency: %w", ErrInvalidLine, i, err)
		}
	}
	return checkBalanced(req.Lines)
}

// checkBalanced implements FIN-10 in code: debits must equal credits in each
// currency. Multi-currency transactions are allowed when every currency
// balances on its own.
func checkBalanced(lines []Line) error {
	totals := make(map[string]int64)
	for _, line := range lines {
		delta := line.AmountMinor
		if line.Direction == Credit {
			delta = -delta
		}
		sum := totals[line.Currency]
		if (delta > 0 && sum > math.MaxInt64-delta) || (delta < 0 && sum < math.MinInt64-delta) {
			return fmt.Errorf("%w: amount overflow in %s", ErrInvalidLine, line.Currency)
		}
		totals[line.Currency] = sum + delta
	}
	var unbalanced []string
	for currency, total := range totals {
		if total != 0 {
			unbalanced = append(unbalanced, currency)
		}
	}
	if len(unbalanced) > 0 {
		sort.Strings(unbalanced)
		return fmt.Errorf("%w: debits do not equal credits in %s", ErrUnbalanced, strings.Join(unbalanced, ", "))
	}
	return nil
}

// accountRow is the policy-relevant projection of accounts.
type accountRow struct {
	Code     string
	Currency string
	Status   string
}

// loadAccounts reads every account referenced by lines in one query.
func loadAccounts(ctx context.Context, tx pgx.Tx, lines []Line) (map[string]accountRow, error) {
	ids := make([]string, 0, len(lines))
	seen := make(map[string]bool, len(lines))
	for _, line := range lines {
		if !seen[line.AccountID] {
			seen[line.AccountID] = true
			ids = append(ids, line.AccountID)
		}
	}

	rows, err := tx.Query(ctx,
		`SELECT id::text, code, currency, status
		   FROM accounts
		  WHERE id = ANY(($1::text[])::uuid[])`, ids)
	if err != nil {
		return nil, fmt.Errorf("ledger: load accounts: %w", err)
	}
	defer rows.Close()

	accounts := make(map[string]accountRow, len(ids))
	for rows.Next() {
		var (
			id  string
			row accountRow
		)
		if err := rows.Scan(&id, &row.Code, &row.Currency, &row.Status); err != nil {
			return nil, fmt.Errorf("ledger: scan account: %w", err)
		}
		accounts[id] = row
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: iterate accounts: %w", err)
	}
	return accounts, nil
}

// checkAccounts applies FIN-13 in code: active accounts only, the account's
// currency, adjustment allowlist, and customer-funds T2.
func (s *Service) checkAccounts(kind Kind, lines []Line, accounts map[string]accountRow) error {
	for _, line := range lines {
		account, ok := accounts[line.AccountID]
		if !ok {
			return fmt.Errorf("%w: %s", ErrAccountNotFound, line.AccountID)
		}
		if account.Status != "active" {
			return fmt.Errorf("%w: account %s is %s", ErrAccountNotPostable, account.Code, account.Status)
		}
		if account.Currency != line.Currency {
			return fmt.Errorf("%w: account %s is %s, entry is %s",
				ErrEntryCurrencyMismatch, account.Code, account.Currency, line.Currency)
		}
		if s.Config.IsCustomerFunds(account.Code) {
			return fmt.Errorf("%w: account %s", ErrCustomerFundsT2, account.Code)
		}
		if kind == KindAdjustment && !s.Config.AllowsAdjustment(account.Code) {
			return fmt.Errorf("%w: account %s", ErrAdjustmentNotAllowed, account.Code)
		}
	}
	return nil
}

// storedTxn is a committed ledger_txns row loaded for replay.
type storedTxn struct {
	ID              string
	Seq             int64
	Kind            string
	ValueDate       time.Time
	CreatedAt       time.Time
	BookedAt        time.Time
	PeriodID        string
	AdjustsPeriodID *string
	ReversalOf      *string
	IdempotencyKey  string
	RequestHash     string
}

func lookupTxnByKey(ctx context.Context, tx pgx.Tx, key string) (storedTxn, error) {
	var (
		stored       storedTxn
		adjusts      string
		reversalOf   string
		requestHash  string
		idempotencyK string
	)
	err := tx.QueryRow(ctx,
		`SELECT id::text, seq, kind, value_date, created_at, booked_at,
		        period_id::text, COALESCE(adjusts_period_id::text, ''),
		        COALESCE(reversal_of::text, ''), idempotency_key, request_hash
		   FROM ledger_txns
		  WHERE idempotency_key = $1`, key).
		Scan(&stored.ID, &stored.Seq, &stored.Kind, &stored.ValueDate,
			&stored.CreatedAt, &stored.BookedAt, &stored.PeriodID, &adjusts,
			&reversalOf, &idempotencyK, &requestHash)
	if err != nil {
		return storedTxn{}, err
	}
	stored.AdjustsPeriodID = stringPtrOrNil(adjusts)
	stored.ReversalOf = stringPtrOrNil(reversalOf)
	stored.IdempotencyKey = idempotencyK
	stored.RequestHash = requestHash
	return stored, nil
}

func replayStored(stored storedTxn, hash string) (Result, error) {
	if stored.RequestHash != hash {
		return Result{}, fmt.Errorf("%w: key %q was used with request_hash %s, request has %s",
			ErrIdempotencyConflict, stored.IdempotencyKey, stored.RequestHash, hash)
	}
	return Result{
		ID:              stored.ID,
		Seq:             stored.Seq,
		Kind:            Kind(stored.Kind),
		ValueDate:       stored.ValueDate.UTC(),
		CreatedAt:       stored.CreatedAt,
		BookedAt:        stored.BookedAt,
		PeriodID:        stored.PeriodID,
		AdjustsPeriodID: stored.AdjustsPeriodID,
		ReversalOf:      stored.ReversalOf,
		IdempotencyKey:  stored.IdempotencyKey,
		RequestHash:     stored.RequestHash,
		Replayed:        true,
	}, nil
}

// hasReversal reports whether txnID already has a reversal.
func hasReversal(ctx context.Context, tx pgx.Tx, txnID string) (bool, error) {
	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM ledger_txns WHERE reversal_of = $1::uuid)`, txnID).Scan(&exists); err != nil {
		return false, fmt.Errorf("ledger: check existing reversal: %w", err)
	}
	return exists, nil
}

// auditPayload is the after-state written to audit_log. Amounts stay integer
// minor units (FIN-05) and only ids/amounts are included.
func auditPayload(result Result, lines []Line) map[string]any {
	auditLines := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		auditLines = append(auditLines, map[string]any{
			"account_id":   line.AccountID,
			"direction":    string(line.Direction),
			"amount_minor": line.AmountMinor,
			"currency":     line.Currency,
		})
	}
	return map[string]any{
		"id":                result.ID,
		"seq":               result.Seq,
		"kind":              string(result.Kind),
		"value_date":        result.ValueDate.UTC().Format(dateLayout),
		"period_id":         result.PeriodID,
		"adjusts_period_id": anyOrNil(result.AdjustsPeriodID),
		"reversal_of":       anyOrNil(result.ReversalOf),
		"idempotency_key":   result.IdempotencyKey,
		"request_hash":      result.RequestHash,
		"lines":             auditLines,
	}
}

func entryMetadata(metadata map[string]any) (string, error) {
	if metadata == nil {
		return "{}", nil
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// dateOnlyValue normalizes a timestamp to midnight UTC on its business date.
func dateOnlyValue(t time.Time) time.Time {
	utc := t.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableStringPtr(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func stringPtrOrNil(value string) *string {
	if value == "" {
		return nil
	}
	copied := value
	return &copied
}

func anyOrNil(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}
