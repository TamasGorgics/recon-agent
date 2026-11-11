package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TamasGorgics/recon-agent/internal/cases"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/match"
)

// PostgresStore implements the API read model through the recon_app pool.
type PostgresStore struct {
	pool *pgxpool.Pool
}

// NewPostgresStore builds the read model.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

// ListCases returns one keyset page ordered by created_at DESC, id DESC.
func (s *PostgresStore) ListCases(ctx context.Context, query CaseListQuery) (CaseListPage, error) {
	args := make([]any, 0, 4)
	sql := `SELECT c.id::text, c.priority, c.value_minor, c.currency, c.status,
	               c.version, c.attempts, c.created_at,
	               COALESCE(array_agg(cb.break_id::text ORDER BY cb.break_id)
	                        FILTER (WHERE cb.break_id IS NOT NULL), ARRAY[]::text[]) AS break_ids
	          FROM cases c
	          LEFT JOIN case_breaks cb ON cb.case_id = c.id
	         WHERE 1 = 1`
	if query.Status != "" {
		args = append(args, query.Status)
		sql += fmt.Sprintf(" AND c.status = $%d", len(args))
	}
	if query.Cursor != nil {
		args = append(args, query.Cursor.CreatedAt, query.Cursor.ID)
		sql += fmt.Sprintf(" AND (c.created_at, c.id) < ($%d::timestamptz, $%d::uuid)",
			len(args)-1, len(args))
	}
	args = append(args, query.Limit+1)
	sql += fmt.Sprintf(` GROUP BY c.id ORDER BY c.created_at DESC, c.id DESC LIMIT $%d`, len(args))

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return CaseListPage{}, fmt.Errorf("api: query cases: %w", err)
	}
	defer rows.Close()

	items := make([]CaseSummary, 0, query.Limit+1)
	for rows.Next() {
		var item CaseSummary
		if err := rows.Scan(&item.ID, &item.Priority, &item.ValueMinor, &item.Currency,
			&item.Status, &item.Version, &item.Attempts, &item.CreatedAt, &item.BreakIDs); err != nil {
			return CaseListPage{}, fmt.Errorf("api: scan case: %w", err)
		}
		item.CreatedAt = item.CreatedAt.UTC()
		if item.BreakIDs == nil {
			item.BreakIDs = []string{}
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return CaseListPage{}, fmt.Errorf("api: read cases: %w", err)
	}

	page := CaseListPage{Items: items}
	if len(items) > query.Limit {
		page.Items = items[:query.Limit]
		last := page.Items[len(page.Items)-1]
		page.NextCursor = encodeCursor(last.CreatedAt, last.ID)
	}
	return page, nil
}

// ListProposals returns one summary-only keyset page.
func (s *PostgresStore) ListProposals(ctx context.Context, query ProposalListQuery) (ProposalListPage, error) {
	args := make([]any, 0, 5)
	sql := `SELECT id::text, case_id::text, type, status, risk_tier, confidence::text,
	               pattern_claimed, pattern_verified, created_at, expires_at
	          FROM proposals
	         WHERE 1 = 1`
	if query.Status != "" {
		args = append(args, query.Status)
		sql += fmt.Sprintf(" AND status = $%d", len(args))
	}
	if query.CaseID != "" {
		args = append(args, query.CaseID)
		sql += fmt.Sprintf(" AND case_id = $%d::uuid", len(args))
	}
	if query.Cursor != nil {
		args = append(args, query.Cursor.CreatedAt, query.Cursor.ID)
		sql += fmt.Sprintf(" AND (created_at, id) < ($%d::timestamptz, $%d::uuid)",
			len(args)-1, len(args))
	}
	args = append(args, query.Limit+1)
	sql += fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT $%d`, len(args))

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return ProposalListPage{}, fmt.Errorf("api: query proposals: %w", err)
	}
	defer rows.Close()

	items := make([]ProposalSummary, 0, query.Limit+1)
	for rows.Next() {
		var item ProposalSummary
		var confidence string
		if err := rows.Scan(&item.ID, &item.CaseID, &item.Type, &item.Status,
			&item.RiskTier, &confidence, &item.PatternClaimed, &item.PatternVerified,
			&item.CreatedAt, &item.ExpiresAt); err != nil {
			return ProposalListPage{}, fmt.Errorf("api: scan proposal: %w", err)
		}
		item.CreatedAt = item.CreatedAt.UTC()
		item.Confidence, err = strconv.ParseFloat(strings.TrimSpace(confidence), 64)
		if err != nil {
			return ProposalListPage{}, fmt.Errorf("api: parse proposal confidence: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return ProposalListPage{}, fmt.Errorf("api: read proposals: %w", err)
	}

	page := ProposalListPage{Items: items}
	if len(items) > query.Limit {
		page.Items = items[:query.Limit]
		last := page.Items[len(page.Items)-1]
		page.NextCursor = encodeCursor(last.CreatedAt, last.ID)
	}
	return page, nil
}

// GetCaseFile reads all case-file sections in one read-only, repeatable-read
// transaction so the document is internally consistent.
func (s *PostgresStore) GetCaseFile(ctx context.Context, caseID string) (CaseFile, error) {
	var document CaseFile
	err := db.WithTx(ctx, s.pool, pgx.TxOptions{
		AccessMode: pgx.ReadOnly,
		IsoLevel:   pgx.RepeatableRead,
	}, func(ctx context.Context, tx pgx.Tx) error {
		var fileCase CaseFileCase
		if err := tx.QueryRow(ctx,
			`SELECT id::text, priority, value_minor, currency, status, version,
			        attempts, escalation_reason, created_at, resolved_at, resolution_type
			   FROM cases WHERE id = $1::uuid`, caseID).
			Scan(&fileCase.ID, &fileCase.Priority, &fileCase.ValueMinor, &fileCase.Currency,
				&fileCase.Status, &fileCase.Version, &fileCase.Attempts,
				&fileCase.EscalationReason, &fileCase.CreatedAt, &fileCase.ResolvedAt,
				&fileCase.ResolutionType); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return cases.ErrCaseNotFound
			}
			return fmt.Errorf("api: read case: %w", err)
		}
		fileCase.CreatedAt = fileCase.CreatedAt.UTC()
		if fileCase.ResolvedAt != nil {
			utc := fileCase.ResolvedAt.UTC()
			fileCase.ResolvedAt = &utc
		}

		breakIDs, err := loadCaseBreakIDs(ctx, tx, caseID)
		if err != nil {
			return err
		}
		fileCase.BreakIDs = breakIDs

		breaks, err := loadCaseBreaks(ctx, tx, caseID)
		if err != nil {
			return err
		}
		proposals, err := loadCaseProposals(ctx, tx, caseID)
		if err != nil {
			return err
		}
		runs, err := loadCaseAgentRuns(ctx, tx, caseID)
		if err != nil {
			return err
		}
		document = CaseFile{
			SchemaVersion: 1,
			Case:          fileCase,
			Breaks:        breaks,
			Proposals:     proposals,
			AgentRuns:     runs,
		}
		return nil
	})
	if err != nil {
		return CaseFile{}, err
	}
	return document, nil
}

func loadCaseBreakIDs(ctx context.Context, tx pgx.Tx, caseID string) ([]string, error) {
	rows, err := tx.Query(ctx,
		`SELECT break_id::text FROM case_breaks WHERE case_id = $1::uuid ORDER BY break_id`, caseID)
	if err != nil {
		return nil, fmt.Errorf("api: query case break ids: %w", err)
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("api: scan case break id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("api: read case break ids: %w", err)
	}
	return ids, nil
}

func loadCaseBreaks(ctx context.Context, tx pgx.Tx, caseID string) ([]Break, error) {
	rows, err := tx.Query(ctx,
		`SELECT b.id::text, b.signature, b.scope_id::text, b.match_run_id::text, b.side,
		        b.ledger_txn_ids::text[], b.external_refs, b.delta_minor, b.currency,
		        b.status, b.detected_at
		   FROM breaks b
		   JOIN case_breaks cb ON cb.break_id = b.id
		  WHERE cb.case_id = $1::uuid
		  ORDER BY b.detected_at, b.id`, caseID)
	if err != nil {
		return nil, fmt.Errorf("api: query case breaks: %w", err)
	}
	defer rows.Close()

	breaks := make([]Break, 0)
	for rows.Next() {
		var item Break
		var refsJSON []byte
		if err := rows.Scan(&item.ID, &item.Signature, &item.ScopeID, &item.MatchRunID,
			&item.Side, &item.LedgerTxnIDs, &refsJSON, &item.DeltaMinor, &item.Currency,
			&item.Status, &item.DetectedAt); err != nil {
			return nil, fmt.Errorf("api: scan case break: %w", err)
		}
		item.DetectedAt = item.DetectedAt.UTC()
		if err := json.Unmarshal(refsJSON, &item.ExternalRefs); err != nil {
			return nil, fmt.Errorf("api: decode break refs: %w", err)
		}
		if item.LedgerTxnIDs == nil {
			item.LedgerTxnIDs = []string{}
		}
		if item.ExternalRefs == nil {
			item.ExternalRefs = []match.RecordRef{}
		}
		breaks = append(breaks, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("api: read case breaks: %w", err)
	}
	return breaks, nil
}

func loadCaseProposals(ctx context.Context, tx pgx.Tx, caseID string) ([]ProposalDetail, error) {
	rows, err := tx.Query(ctx,
		`SELECT id::text, type, status, risk_tier, confidence::text, pattern_claimed,
		        pattern_verified, rationale, evidence, created_at, expires_at
		   FROM proposals
		  WHERE case_id = $1::uuid
		  ORDER BY created_at, id`, caseID)
	if err != nil {
		return nil, fmt.Errorf("api: query case proposals: %w", err)
	}
	defer rows.Close()

	proposals := make([]ProposalDetail, 0)
	for rows.Next() {
		var item ProposalDetail
		var confidence string
		var evidenceJSON []byte
		if err := rows.Scan(&item.ID, &item.Type, &item.Status, &item.RiskTier,
			&confidence, &item.PatternClaimed, &item.PatternVerified, &item.Rationale,
			&evidenceJSON, &item.CreatedAt, &item.ExpiresAt); err != nil {
			return nil, fmt.Errorf("api: scan case proposal: %w", err)
		}
		item.CreatedAt = item.CreatedAt.UTC()
		item.Confidence, err = strconv.ParseFloat(strings.TrimSpace(confidence), 64)
		if err != nil {
			return nil, fmt.Errorf("api: parse proposal confidence: %w", err)
		}
		item.Rationale = sanitizeRationale(item.Rationale)
		if err := json.Unmarshal(evidenceJSON, &item.EvidenceRefs); err != nil {
			return nil, fmt.Errorf("api: decode proposal evidence: %w", err)
		}
		if item.EvidenceRefs == nil {
			item.EvidenceRefs = []string{}
		}
		proposals = append(proposals, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("api: read case proposals: %w", err)
	}
	return proposals, nil
}

func loadCaseAgentRuns(ctx context.Context, tx pgx.Tx, caseID string) ([]AgentRun, error) {
	rows, err := tx.Query(ctx,
		`SELECT id::text, model, prompt_version, started_at, ended_at, outcome,
		        tokens_in, tokens_out, cost_usd::text, trace_id
		   FROM agent_runs
		  WHERE case_id = $1::uuid
		  ORDER BY started_at, id`, caseID)
	if err != nil {
		return nil, fmt.Errorf("api: query case agent runs: %w", err)
	}
	defer rows.Close()

	runs := make([]AgentRun, 0)
	for rows.Next() {
		var item AgentRun
		if err := rows.Scan(&item.ID, &item.Model, &item.PromptVersion, &item.StartedAt,
			&item.EndedAt, &item.Outcome, &item.TokensIn, &item.TokensOut,
			&item.CostUSD, &item.TraceID); err != nil {
			return nil, fmt.Errorf("api: scan case agent run: %w", err)
		}
		item.StartedAt = item.StartedAt.UTC()
		if item.EndedAt != nil {
			utc := item.EndedAt.UTC()
			item.EndedAt = &utc
		}
		runs = append(runs, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("api: read case agent runs: %w", err)
	}
	return runs, nil
}

// maxRationaleRunes caps the case-file rationale at 4000 characters.
const maxRationaleRunes = 4000

// sanitizeRationale removes control characters and truncates to 4000 runes.
func sanitizeRationale(value string) string {
	var builder strings.Builder
	count := 0
	for _, r := range value {
		if unicode.IsControl(r) {
			continue
		}
		if count >= maxRationaleRunes {
			break
		}
		builder.WriteRune(r)
		count++
	}
	return builder.String()
}

// Ensure the store satisfies the handler interfaces at compile time.
var (
	_ CaseReader     = (*PostgresStore)(nil)
	_ ProposalReader = (*PostgresStore)(nil)
)
