package cases

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/match"
)

// CreateBreaksRequest records the unmatched remainder of one matcher run.
type CreateBreaksRequest struct {
	ScopeID    string
	MatchRunID string
	Breaks     []match.BreakCandidate
}

// CreateBreaksResult reports idempotent creation. CreatedBreaks counts rows
// inserted by this call; ExistingBreaks counts candidates whose signature was
// already present. Cases are only the cases created by this call.
type CreateBreaksResult struct {
	Breaks         []Break
	CreatedBreaks  int
	ExistingBreaks int
	Cases          []Case
}

const breakColumns = `id::text, signature, scope_id::text, match_run_id::text, side,
	ledger_txn_ids::text[], external_refs, delta_minor, currency, status, detected_at`

func scanBreak(row caseScanner) (Break, error) {
	var (
		b        Break
		side     string
		status   string
		refsJSON []byte
	)
	if err := row.Scan(&b.ID, &b.Signature, &b.ScopeID, &b.MatchRunID, &side,
		&b.LedgerTxnIDs, &refsJSON, &b.DeltaMinor, &b.Currency, &status,
		&b.DetectedAt); err != nil {
		return Break{}, err
	}
	b.Side = match.BreakSide(side)
	b.Status = BreakStatus(status)
	if err := json.Unmarshal(refsJSON, &b.ExternalRefs); err != nil {
		return Break{}, fmt.Errorf("cases: decode break refs: %w", err)
	}
	if b.ExternalRefs == nil {
		b.ExternalRefs = []match.RecordRef{}
	}
	if b.LedgerTxnIDs == nil {
		b.LedgerTxnIDs = []string{}
	}
	return b, nil
}

func (s *Service) loadBreaksForUpdate(ctx context.Context, tx pgx.Tx, ids []string) ([]Break, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+breakColumns+` FROM breaks
		 WHERE id::text = ANY($1::text[]) ORDER BY id FOR UPDATE`, ids)
	if err != nil {
		return nil, fmt.Errorf("cases: load breaks: %w", err)
	}
	defer rows.Close()

	breaks := make([]Break, 0, len(ids))
	for rows.Next() {
		b, err := scanBreak(rows)
		if err != nil {
			return nil, fmt.Errorf("cases: scan break: %w", err)
		}
		breaks = append(breaks, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cases: load breaks: %w", err)
	}
	return breaks, nil
}

func (s *Service) loadBreaksBySignatures(ctx context.Context, tx pgx.Tx, signatures []string) ([]Break, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+breakColumns+` FROM breaks
		 WHERE signature = ANY($1::text[]) ORDER BY signature`, signatures)
	if err != nil {
		return nil, fmt.Errorf("cases: load breaks by signature: %w", err)
	}
	defer rows.Close()

	breaks := make([]Break, 0, len(signatures))
	for rows.Next() {
		b, err := scanBreak(rows)
		if err != nil {
			return nil, fmt.Errorf("cases: scan break: %w", err)
		}
		breaks = append(breaks, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cases: load breaks by signature: %w", err)
	}
	return breaks, nil
}

// CreateBreaks records break candidates with ON CONFLICT (signature) DO NOTHING
// and then groups the still-open, unassigned breaks into cases. It is safe to
// call repeatedly with the same candidates and is deterministic under candidate
// order permutations.
func (s *Service) CreateBreaks(ctx context.Context, req CreateBreaksRequest) (CreateBreaksResult, error) {
	if strings.TrimSpace(req.ScopeID) == "" {
		return CreateBreaksResult{}, fmt.Errorf("%w: scope id is required", ErrInvalidRequest)
	}
	if strings.TrimSpace(req.MatchRunID) == "" {
		return CreateBreaksResult{}, fmt.Errorf("%w: match run id is required", ErrInvalidRequest)
	}

	candidates, err := dedupeCandidates(req.Breaks, req.ScopeID)
	if err != nil {
		return CreateBreaksResult{}, err
	}
	if len(candidates) == 0 {
		return CreateBreaksResult{}, nil
	}
	now := s.clock.Now().UTC().Truncate(time.Microsecond)

	var result CreateBreaksResult
	err = db.WithTx(ctx, s.db, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		signatures := make([]string, 0, len(candidates))
		created := 0
		for _, candidate := range candidates {
			wasCreated, err := s.insertBreakTx(ctx, tx, req.MatchRunID, candidate, now)
			if err != nil {
				return err
			}
			if wasCreated {
				created++
			}
			signatures = append(signatures, candidate.Signature())
		}
		persisted, err := s.loadBreaksBySignatures(ctx, tx, signatures)
		if err != nil {
			return err
		}
		result.Breaks = persisted
		result.CreatedBreaks = created
		result.ExistingBreaks = len(persisted) - created

		unassigned, err := s.openUnassignedBreaks(ctx, tx, persisted)
		if err != nil {
			return err
		}
		batches, err := s.loadBatchIDs(ctx, tx, externalRefsOf(unassigned))
		if err != nil {
			return err
		}
		groups := groupBreaks(unassigned, batches)
		createdCases := make([]Case, 0, len(groups))
		for _, group := range groups {
			ids := make([]string, 0, len(group.breaks))
			for _, b := range group.breaks {
				ids = append(ids, b.ID)
			}
			c, err := s.createCaseTx(ctx, tx, ids, group.key)
			if err != nil {
				return err
			}
			createdCases = append(createdCases, c)
		}
		result.Cases = createdCases
		return nil
	})
	if err != nil {
		return CreateBreaksResult{}, err
	}
	return result, nil
}

// insertBreakTx inserts one canonical candidate and reports whether it was new.
// On a signature conflict the existing row is returned and nothing is written;
// that is D-5's no-reopen rule.
func (s *Service) insertBreakTx(ctx context.Context, tx pgx.Tx, runID string, candidate match.BreakCandidate, now time.Time) (bool, error) {
	ledgerIDs := candidate.LedgerTxnIDs
	if ledgerIDs == nil {
		ledgerIDs = []string{}
	}
	refs := candidate.ExternalRecords
	if refs == nil {
		refs = []match.RecordRef{}
	}
	refsJSON, err := json.Marshal(refs)
	if err != nil {
		return false, fmt.Errorf("cases: encode break refs: %w", err)
	}

	var id string
	err = tx.QueryRow(ctx,
		`INSERT INTO breaks
		     (signature, scope_id, match_run_id, side, ledger_txn_ids,
		      external_refs, delta_minor, currency, status, detected_at)
		 VALUES ($1, $2::uuid, $3::uuid, $4, $5::text[]::uuid[],
		         $6::jsonb, $7, $8, 'open', $9)
		 ON CONFLICT (signature) DO NOTHING
		 RETURNING id::text`,
		candidate.Signature(), candidate.ScopeID, runID, string(candidate.Side),
		ledgerIDs, string(refsJSON), candidate.DeltaMinor, candidate.Currency, now).
		Scan(&id)
	if err == nil {
		if _, err := audit.Append(ctx, tx, audit.Entry{
			Actor:      s.actor,
			Action:     ActionBreakCreate,
			EntityType: EntityBreak,
			EntityID:   id,
			After: map[string]any{
				"signature":    candidate.Signature(),
				"scope_id":     candidate.ScopeID,
				"match_run_id": runID,
				"side":         string(candidate.Side),
				"delta_minor":  candidate.DeltaMinor,
				"currency":     candidate.Currency,
				"status":       string(BreakStatusOpen),
			},
			Timestamp: now,
		}); err != nil {
			return false, err
		}
		return true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("cases: insert break %s: %w", candidate.Signature(), err)
	}
	if err := tx.QueryRow(ctx,
		`SELECT id::text FROM breaks WHERE signature = $1`, candidate.Signature()).
		Scan(&id); err != nil {
		return false, fmt.Errorf("cases: read conflicting break: %w", err)
	}
	return false, nil
}

func (s *Service) openUnassignedBreaks(ctx context.Context, tx pgx.Tx, breaks []Break) ([]Break, error) {
	if len(breaks) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(breaks))
	for _, b := range breaks {
		if b.Status == BreakStatusOpen {
			ids = append(ids, b.ID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx,
		`SELECT break_id::text FROM case_breaks WHERE break_id::text = ANY($1::text[])`, ids)
	if err != nil {
		return nil, fmt.Errorf("cases: check assigned breaks: %w", err)
	}
	defer rows.Close()

	assigned := make(map[string]struct{}, len(ids))
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("cases: scan assigned break: %w", err)
		}
		assigned[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cases: check assigned breaks: %w", err)
	}

	out := make([]Break, 0, len(ids))
	for _, b := range breaks {
		if b.Status != BreakStatusOpen {
			continue
		}
		if _, ok := assigned[b.ID]; ok {
			continue
		}
		out = append(out, b)
	}
	return out, nil
}

func externalRefsOf(breaks []Break) []match.RecordRef {
	refs := make([]match.RecordRef, 0)
	for _, b := range breaks {
		refs = append(refs, b.ExternalRefs...)
	}
	return refs
}

func (s *Service) loadBatchIDs(ctx context.Context, tx pgx.Tx, refs []match.RecordRef) (map[match.RecordRef]string, error) {
	result := make(map[match.RecordRef]string, len(refs))
	sources := make([]string, 0, len(refs))
	ids := make([]string, 0, len(refs))
	for _, ref := range refs {
		if ref.Source == "" || ref.SourceRecordID == "" {
			continue
		}
		sources = append(sources, ref.Source)
		ids = append(ids, ref.SourceRecordID)
	}
	if len(sources) == 0 {
		return result, nil
	}
	rows, err := tx.Query(ctx,
		`SELECT r.source, r.source_record_id, er.batch_id::text
		   FROM unnest($1::text[], $2::text[]) AS r(source, source_record_id)
		   JOIN external_records er
		     ON er.source = r.source AND er.source_record_id = r.source_record_id`,
		sources, ids)
	if err != nil {
		return nil, fmt.Errorf("cases: load external batches: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var source, recordID, batchID string
		if err := rows.Scan(&source, &recordID, &batchID); err != nil {
			return nil, fmt.Errorf("cases: scan external batch: %w", err)
		}
		result[match.RecordRef{Source: source, SourceRecordID: recordID}] = batchID
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cases: load external batches: %w", err)
	}
	return result, nil
}

type breakGroup struct {
	key    string
	breaks []Break
}

// groupBreaks applies the M1-14 grouping rule. A break belongs to a settlement
// batch set only when all of its external refs name the same batch and the
// batch contributes at least two breaks; every other break stands alone.
func groupBreaks(breaks []Break, batches map[match.RecordRef]string) []breakGroup {
	grouped := make(map[string][]Break, len(breaks))
	for _, b := range breaks {
		grouped[breakGroupKey(b, batches)] = append(grouped[breakGroupKey(b, batches)], b)
	}

	final := make(map[string][]Break, len(grouped))
	for key, members := range grouped {
		if strings.HasPrefix(key, "batch:") && len(members) < 2 {
			for _, b := range members {
				final["break:"+b.Signature] = []Break{b}
			}
			continue
		}
		final[key] = members
	}

	keys := make([]string, 0, len(final))
	for key := range final {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	out := make([]breakGroup, 0, len(keys))
	for _, key := range keys {
		members := final[key]
		sort.SliceStable(members, func(i, j int) bool { return members[i].Signature < members[j].Signature })
		out = append(out, breakGroup{key: key, breaks: members})
	}
	return out
}

func breakGroupKey(b Break, batches map[match.RecordRef]string) string {
	if len(b.ExternalRefs) == 0 {
		return "break:" + b.Signature
	}
	batchID := ""
	for _, ref := range b.ExternalRefs {
		id := batches[ref]
		if id == "" {
			return "break:" + b.Signature
		}
		if batchID == "" {
			batchID = id
			continue
		}
		if batchID != id {
			return "break:" + b.Signature
		}
	}
	if batchID == "" {
		return "break:" + b.Signature
	}
	return "batch:" + b.ScopeID + ":" + b.Currency + ":" + batchID
}

// dedupeCandidates validates candidates, de-duplicates by canonical signature,
// and sorts by signature so the write order is independent of match pass order.
func dedupeCandidates(candidates []match.BreakCandidate, scopeID string) ([]match.BreakCandidate, error) {
	seen := make(map[string]struct{}, len(candidates))
	out := make([]match.BreakCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.ScopeID == "" {
			candidate.ScopeID = scopeID
		}
		if candidate.ScopeID != scopeID {
			return nil, fmt.Errorf("%w: break scope %q does not match %q",
				ErrInvalidRequest, candidate.ScopeID, scopeID)
		}
		switch candidate.Side {
		case match.SideLedgerOnly, match.SideExternalOnly, match.SideAmountMismatch,
			match.SideDuplicate:
		default:
			return nil, fmt.Errorf("%w: break side %q", ErrInvalidRequest, candidate.Side)
		}
		if candidate.Currency == "" {
			return nil, fmt.Errorf("%w: break currency is required", ErrInvalidRequest)
		}
		if len(candidate.LedgerTxnIDs) == 0 && len(candidate.ExternalRecords) == 0 {
			return nil, fmt.Errorf("%w: break has no refs", ErrInvalidRequest)
		}
		signature := candidate.Signature()
		if _, duplicate := seen[signature]; duplicate {
			continue
		}
		seen[signature] = struct{}{}
		out = append(out, candidate)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Signature() < out[j].Signature()
	})
	return out, nil
}
