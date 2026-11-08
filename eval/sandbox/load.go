package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/cases"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/match"
	"github.com/TamasGorgics/recon-agent/synth"
	"github.com/TamasGorgics/recon-agent/synth/modes"
)

// LoadOptions configures one world load.
type LoadOptions struct {
	// Config is the exact dataset.Config used to build the Dataset.
	Config dataset.Config
	// Clock is the fixed world-load clock. Nil means
	// clock.NewFake(Manager.BaseClock). clock.Real is rejected.
	Clock clock.Clock
	// Actor is the audit actor for synth.Load. The zero value defaults to
	// system:eval.
	Actor audit.Actor
	// Match is the matcher configuration. The zero value selects
	// config.DefaultMatchConfig through match.New.
	Match config.MatchConfig
}

func validateCloneInputs(ds *dataset.Dataset, load LoadOptions) error {
	if ds == nil {
		return fmt.Errorf("%w: dataset is required", ErrInvalidLoad)
	}
	if !load.Config.Split.Valid() {
		return fmt.Errorf("%w: split %q is not dev, test, or injection",
			ErrInvalidLoad, load.Config.Split)
	}
	if ds.Manifest.Split != load.Config.Split {
		return fmt.Errorf("%w: dataset split %q does not match config split %q",
			ErrInvalidLoad, ds.Manifest.Split, load.Config.Split)
	}
	if strings.TrimSpace(ds.Manifest.EvalDatasetVersion) == "" {
		return fmt.Errorf("%w: dataset has no eval_dataset_version", ErrInvalidLoad)
	}
	if load.Config.Days <= 0 {
		return fmt.Errorf("%w: config days must be positive", ErrInvalidLoad)
	}
	if strings.TrimSpace(load.Config.Synth.BaseDate) == "" {
		return fmt.Errorf("%w: config synth base date is required", ErrInvalidLoad)
	}
	if err := dataset.ValidateGroundTruth(ds.Truth); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidLoad, err)
	}
	if ds.Manifest.TruthSchemaVersion != "" && ds.Manifest.TruthSchemaVersion != ds.Truth.SchemaVersion {
		return fmt.Errorf("%w: manifest truth schema %q does not match ground truth %q",
			ErrInvalidLoad, ds.Manifest.TruthSchemaVersion, ds.Truth.SchemaVersion)
	}
	return nil
}

func resolveLoadClock(provided clock.Clock, base time.Time) (clock.Clock, error) {
	if provided == nil {
		return clock.NewFake(base), nil
	}
	switch provided.(type) {
	case clock.Real, *clock.Real:
		return nil, fmt.Errorf("%w: the world-load clock must be deterministic; clock.Real is not allowed",
			ErrInvalidLoad)
	default:
		return provided, nil
	}
}

func (s *Sandbox) loadWorld(ctx context.Context, ds *dataset.Dataset, load LoadOptions) error {
	loadClock, err := resolveLoadClock(load.Clock, s.baseClock())
	if err != nil {
		return err
	}
	actor := load.Actor
	if !actor.Valid() {
		actor, err = audit.SystemActor("eval")
		if err != nil {
			return fmt.Errorf("sandbox: build eval actor: %w", err)
		}
	}

	dir, err := os.MkdirTemp("", "recon-eval-world-*")
	if err != nil {
		return fmt.Errorf("sandbox: create temp world directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := ds.WriteWorld(dir); err != nil {
		return fmt.Errorf("sandbox: write temp world: %w", err)
	}

	if _, err := synth.Load(ctx, synth.LoadOptions{
		Dir:     dir,
		DB:      s.Admin,
		Clock:   loadClock,
		Actor:   actor,
		Sources: config.IngestConfig{Sources: load.Config.Sources},
		Ledger:  load.Config.Ledger,
	}); err != nil {
		return fmt.Errorf("sandbox: load generated world: %w", err)
	}

	scopes, err := s.loadScopes(ctx)
	if err != nil {
		return err
	}
	scopes, err = s.pinScopeIDs(ctx, ds.Manifest.Seed, scopes)
	if err != nil {
		return err
	}

	runIDs, breakIndex, err := s.runMatcher(ctx, load, loadClock, scopes)
	if err != nil {
		return err
	}

	refs, truthByID, err := s.createCases(ctx, ds, loadClock, scopes, runIDs, breakIndex)
	if err != nil {
		return err
	}
	refs, err = s.computeScenarioTimes(ctx, refs, truthByID)
	if err != nil {
		return err
	}
	if err := s.persistTruth(ctx, ds, loadClock, refs, truthByID); err != nil {
		return err
	}
	return nil
}

// baseClock is the deterministic fallback instant pinned by the Manager.
func (s *Sandbox) baseClock() time.Time {
	if s.base.IsZero() {
		return dataset.DefaultGeneratedAt
	}
	return s.base
}

func (s *Sandbox) loadScopes(ctx context.Context) ([]scopeRow, error) {
	rows, err := s.Admin.Query(ctx,
		`SELECT id::text, source, currency
		   FROM reconciliation_scopes
		  ORDER BY source, currency`)
	if err != nil {
		return nil, fmt.Errorf("sandbox: load reconciliation scopes: %w", err)
	}
	defer rows.Close()

	scopes := make([]scopeRow, 0)
	for rows.Next() {
		var scope scopeRow
		if err := rows.Scan(&scope.ID, &scope.Source, &scope.Currency); err != nil {
			return nil, fmt.Errorf("sandbox: scan reconciliation scope: %w", err)
		}
		scopes = append(scopes, scope)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sandbox: iterate reconciliation scopes: %w", err)
	}
	if len(scopes) == 0 {
		return nil, fmt.Errorf("%w: world loaded no reconciliation scopes", ErrInvalidLoad)
	}
	return scopes, nil
}

// pinScopeIDs rewrites the Postgres-generated scope UUIDs to the dataset's
// deterministic logical ids so committed truth signatures are reproducible.
// statement_balances is the only table that references scopes before the
// matcher runs, so the rewrite inserts a temporary duplicate, moves the
// balance rows, removes the old scope, and restores the source. The whole
// move is one transaction.
func (s *Sandbox) pinScopeIDs(ctx context.Context, seed int64, scopes []scopeRow) ([]scopeRow, error) {
	updated := make([]scopeRow, 0, len(scopes))
	err := db.WithTx(ctx, s.Admin, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		for _, scope := range scopes {
			deterministic := deterministicScopeID(seed, scope.Source, scope.Currency)
			if deterministic == scope.ID {
				updated = append(updated, scope)
				continue
			}
			temporarySource := scope.Source + "__eval_scope_tmp"
			if _, err := tx.Exec(ctx,
				`INSERT INTO reconciliation_scopes (id, ledger_account_id, source, currency)
				 SELECT $1::uuid, ledger_account_id, $2, $3
				   FROM reconciliation_scopes WHERE id = $4::uuid`,
				deterministic, temporarySource, scope.Currency, scope.ID); err != nil {
				return fmt.Errorf("sandbox: stage deterministic scope %s/%s: %w",
					scope.Source, scope.Currency, err)
			}
			if _, err := tx.Exec(ctx,
				`UPDATE statement_balances SET scope_id = $1::uuid WHERE scope_id = $2::uuid`,
				deterministic, scope.ID); err != nil {
				return fmt.Errorf("sandbox: move statement balances to deterministic scope %s/%s: %w",
					scope.Source, scope.Currency, err)
			}
			if _, err := tx.Exec(ctx,
				`DELETE FROM reconciliation_scopes WHERE id = $1::uuid`, scope.ID); err != nil {
				return fmt.Errorf("sandbox: remove random scope %s/%s: %w",
					scope.Source, scope.Currency, err)
			}
			if _, err := tx.Exec(ctx,
				`UPDATE reconciliation_scopes SET source = $1 WHERE id = $2::uuid`,
				scope.Source, deterministic); err != nil {
				return fmt.Errorf("sandbox: restore deterministic scope source %s/%s: %w",
					scope.Source, scope.Currency, err)
			}
			updated = append(updated, scopeRow{
				ID:       deterministic,
				Source:   scope.Source,
				Currency: scope.Currency,
			})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("sandbox: pin deterministic scope ids: %w", err)
	}
	return updated, nil
}

func (s *Sandbox) runMatcher(ctx context.Context, load LoadOptions, loadClock clock.Clock,
	scopes []scopeRow) (map[string]string, map[string][]match.BreakCandidate, error) {
	base, err := time.ParseInLocation("2006-01-02", load.Config.Synth.BaseDate, time.UTC)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: synth base date %q: %v", ErrInvalidLoad,
			load.Config.Synth.BaseDate, err)
	}
	window := match.Window{
		From: base.AddDate(0, 0, -7),
		To:   base.AddDate(0, 0, load.Config.Days+60),
	}
	matcherActor, err := audit.SystemActor("matcher")
	if err != nil {
		return nil, nil, fmt.Errorf("sandbox: build matcher actor: %w", err)
	}
	engine, err := match.New(match.Options{
		DB:     s.Admin,
		Clock:  loadClock,
		Actor:  matcherActor,
		Config: load.Match,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("sandbox: create matcher: %w", err)
	}

	runIDs := make(map[string]string, len(scopes))
	breakIndex := make(map[string][]match.BreakCandidate)
	for _, scope := range scopes {
		result, err := engine.Run(ctx, match.RunRequest{ScopeID: scope.ID, Window: window})
		if err != nil {
			return nil, nil, fmt.Errorf("sandbox: match scope %s/%s: %w",
				scope.Source, scope.Currency, err)
		}
		runIDs[scope.ID] = result.RunID
		for _, candidate := range result.Breaks {
			breakIndex[candidate.Signature()] = append(breakIndex[candidate.Signature()], candidate)
		}
	}
	return runIDs, breakIndex, nil
}

func (s *Sandbox) createCases(ctx context.Context, ds *dataset.Dataset, loadClock clock.Clock,
	scopes []scopeRow, runIDs map[string]string,
	breakIndex map[string][]match.BreakCandidate) ([]CaseRef, map[string]dataset.ScenarioTruth, error) {
	caseActor, err := audit.SystemActor("cases")
	if err != nil {
		return nil, nil, fmt.Errorf("sandbox: build cases actor: %w", err)
	}
	service, err := cases.NewService(s.Admin, loadClock, caseActor)
	if err != nil {
		return nil, nil, fmt.Errorf("sandbox: create case service: %w", err)
	}

	truthScenarios := append([]dataset.ScenarioTruth(nil), ds.Truth.Scenarios...)
	sort.Slice(truthScenarios, func(i, j int) bool {
		return truthScenarios[i].ScenarioID < truthScenarios[j].ScenarioID
	})
	scopesByKey := scopeIndex(scopes)
	truthByID := make(map[string]dataset.ScenarioTruth, len(truthScenarios))
	for _, truth := range truthScenarios {
		truthByID[truth.ScenarioID] = truth
	}

	usedSignatures := make(map[string]string, len(truthScenarios))
	refs := make([]CaseRef, 0, len(truthScenarios))
	for _, truth := range truthScenarios {
		switch {
		case truth.BreakSignature != "":
			if previous, duplicate := usedSignatures[truth.BreakSignature]; duplicate {
				return nil, nil, fmt.Errorf(
					"%w: scenarios %s and %s share signature %s",
					ErrAmbiguousMapping, previous, truth.ScenarioID, truth.BreakSignature)
			}
			usedSignatures[truth.BreakSignature] = truth.ScenarioID

			candidates := breakIndex[truth.BreakSignature]
			if len(candidates) > 1 {
				return nil, nil, fmt.Errorf(
					"%w: scenario %s signature %s has %d matcher candidates",
					ErrAmbiguousMapping, truth.ScenarioID, truth.BreakSignature, len(candidates))
			}
			var candidate match.BreakCandidate
			if len(candidates) == 1 {
				candidate = candidates[0]
			} else {
				candidate, err = reconstructTruthCandidate(truth, scopesByKey)
				if err != nil {
					return nil, nil, err
				}
			}
			runID := runIDs[candidate.ScopeID]
			if runID == "" {
				return nil, nil, fmt.Errorf(
					"%w: scenario %s scope %s has no matcher run",
					ErrInvalidLoad, truth.ScenarioID, candidate.ScopeID)
			}
			result, err := service.CreateBreaks(ctx, cases.CreateBreaksRequest{
				ScopeID:    candidate.ScopeID,
				MatchRunID: runID,
				Breaks:     []match.BreakCandidate{candidate},
			})
			if err != nil {
				return nil, nil, fmt.Errorf("sandbox: create case for scenario %s: %w",
					truth.ScenarioID, err)
			}
			if err := verifyOneCase(result, candidate.Signature(), truth.BreakSignature); err != nil {
				return nil, nil, fmt.Errorf("sandbox: scenario %s: %w", truth.ScenarioID, err)
			}
			refs = append(refs, CaseRef{
				ScenarioID:    truth.ScenarioID,
				CaseID:        result.Cases[0].ID,
				BreakID:       result.Breaks[0].ID,
				caseCreatedAt: result.Cases[0].CreatedAt,
			})

		case truth.Mode == modes.InjectionMemo:
			candidate, err := syntheticCandidate(truth, scopesByKey)
			if err != nil {
				return nil, nil, err
			}
			if previous, duplicate := usedSignatures[candidate.Signature()]; duplicate {
				return nil, nil, fmt.Errorf(
					"%w: synthetic scenario %s collides with scenario %s",
					ErrAmbiguousMapping, truth.ScenarioID, previous)
			}
			usedSignatures[candidate.Signature()] = truth.ScenarioID
			runID := runIDs[candidate.ScopeID]
			if runID == "" {
				return nil, nil, fmt.Errorf(
					"%w: scenario %s scope %s has no matcher run",
					ErrInvalidLoad, truth.ScenarioID, candidate.ScopeID)
			}
			result, err := service.CreateBreaks(ctx, cases.CreateBreaksRequest{
				ScopeID:    candidate.ScopeID,
				MatchRunID: runID,
				Breaks:     []match.BreakCandidate{candidate},
			})
			if err != nil {
				return nil, nil, fmt.Errorf("sandbox: create synthetic case for scenario %s: %w",
					truth.ScenarioID, err)
			}
			if err := verifyOneCase(result, candidate.Signature(), candidate.Signature()); err != nil {
				return nil, nil, fmt.Errorf("sandbox: synthetic scenario %s: %w", truth.ScenarioID, err)
			}
			refs = append(refs, CaseRef{
				ScenarioID:    truth.ScenarioID,
				CaseID:        result.Cases[0].ID,
				Synthetic:     true,
				caseCreatedAt: result.Cases[0].CreatedAt,
			})

		default:
			// Clean scenarios and other empty-signature rows are intentionally
			// not scoreable; they remain in eval_truth.scenarios.
		}
	}
	return refs, truthByID, nil
}

func verifyOneCase(result cases.CreateBreaksResult, candidateSignature, expectedSignature string) error {
	if result.CreatedBreaks != 1 || result.ExistingBreaks != 0 {
		return fmt.Errorf("%w: created=%d existing=%d, want exactly one new break",
			ErrAmbiguousMapping, result.CreatedBreaks, result.ExistingBreaks)
	}
	if len(result.Breaks) != 1 {
		return fmt.Errorf("%w: persisted %d breaks, want 1", ErrAmbiguousMapping, len(result.Breaks))
	}
	if len(result.Cases) != 1 {
		return fmt.Errorf("%w: created %d cases, want 1", ErrAmbiguousMapping, len(result.Cases))
	}
	if result.Cases[0].ID == "" {
		return fmt.Errorf("%w: created case has no id", ErrAmbiguousMapping)
	}
	if result.Breaks[0].ID == "" {
		return fmt.Errorf("%w: persisted break has no id", ErrAmbiguousMapping)
	}
	if result.Breaks[0].Signature != candidateSignature {
		return fmt.Errorf("%w: persisted signature %s, want %s",
			ErrAmbiguousMapping, result.Breaks[0].Signature, candidateSignature)
	}
	if expectedSignature != "" && result.Breaks[0].Signature != expectedSignature {
		return fmt.Errorf("%w: persisted signature %s, truth %s",
			ErrAmbiguousMapping, result.Breaks[0].Signature, expectedSignature)
	}
	return nil
}

func (s *Sandbox) computeScenarioTimes(ctx context.Context, refs []CaseRef,
	truthByID map[string]dataset.ScenarioTruth) ([]CaseRef, error) {
	ledgerEvents, externalEvents, err := s.loadEvents(ctx, refs, truthByID)
	if err != nil {
		return nil, err
	}
	for index := range refs {
		truth := truthByID[refs[index].ScenarioID]
		var ledgerTimes, externalTimes []time.Time
		for _, txnID := range truth.LedgerTxnIDs {
			ledgerTimes = append(ledgerTimes, ledgerEvents[txnID]...)
		}
		for _, ref := range truth.ExternalRecords {
			externalTimes = append(externalTimes,
				externalEvents[scopeKey(ref.Source, ref.SourceRecordID)]...)
		}
		fallback := refs[index].caseCreatedAt
		if refs[index].Synthetic || fallback.IsZero() {
			fallback = s.baseClock()
		}
		refs[index].ScenarioTime = selectScenarioTime(ledgerTimes, externalTimes, fallback)
		if refs[index].ScenarioTime.IsZero() {
			return nil, fmt.Errorf("sandbox: scenario %s has no deterministic scenario time",
				refs[index].ScenarioID)
		}
	}
	return refs, nil
}

func (s *Sandbox) loadEvents(ctx context.Context, refs []CaseRef,
	truthByID map[string]dataset.ScenarioTruth) (map[string][]time.Time, map[string][]time.Time, error) {
	ledgerEvents := make(map[string][]time.Time)
	externalEvents := make(map[string][]time.Time)

	txnIDSet := make(map[string]struct{})
	recordSet := make(map[string]struct{})
	var sources, recordIDs []string
	for _, ref := range refs {
		truth := truthByID[ref.ScenarioID]
		for _, txnID := range truth.LedgerTxnIDs {
			txnIDSet[txnID] = struct{}{}
		}
		for _, record := range truth.ExternalRecords {
			key := scopeKey(record.Source, record.SourceRecordID)
			if _, seen := recordSet[key]; seen {
				continue
			}
			recordSet[key] = struct{}{}
			sources = append(sources, record.Source)
			recordIDs = append(recordIDs, record.SourceRecordID)
		}
	}

	if len(txnIDSet) > 0 {
		txnIDs := make([]string, 0, len(txnIDSet))
		for id := range txnIDSet {
			txnIDs = append(txnIDs, id)
		}
		rows, err := s.Admin.Query(ctx,
			`SELECT id::text, created_at, booked_at
			   FROM ledger_txns WHERE id::text = ANY($1::text[])`, txnIDs)
		if err != nil {
			return nil, nil, fmt.Errorf("sandbox: load ledger event times: %w", err)
		}
		for rows.Next() {
			var id string
			var createdAt, bookedAt time.Time
			if err := rows.Scan(&id, &createdAt, &bookedAt); err != nil {
				rows.Close()
				return nil, nil, fmt.Errorf("sandbox: scan ledger event time: %w", err)
			}
			ledgerEvents[id] = append(ledgerEvents[id], createdAt.UTC(), bookedAt.UTC())
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("sandbox: iterate ledger event times: %w", err)
		}
		rows.Close()
	}

	if len(sources) > 0 {
		rows, err := s.Admin.Query(ctx,
			`SELECT er.source, er.source_record_id, er.occurred_at, er.settled_at
			   FROM unnest($1::text[], $2::text[]) AS r(source, source_record_id)
			   JOIN external_records er
			     ON er.source = r.source AND er.source_record_id = r.source_record_id`,
			sources, recordIDs)
		if err != nil {
			return nil, nil, fmt.Errorf("sandbox: load external event times: %w", err)
		}
		for rows.Next() {
			var source, recordID string
			var occurredAt time.Time
			var settledAt *time.Time
			if err := rows.Scan(&source, &recordID, &occurredAt, &settledAt); err != nil {
				rows.Close()
				return nil, nil, fmt.Errorf("sandbox: scan external event time: %w", err)
			}
			key := scopeKey(source, recordID)
			externalEvents[key] = append(externalEvents[key], occurredAt.UTC())
			if settledAt != nil {
				externalEvents[key] = append(externalEvents[key], settledAt.UTC())
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("sandbox: iterate external event times: %w", err)
		}
		rows.Close()
	}
	return ledgerEvents, externalEvents, nil
}

func (s *Sandbox) persistTruth(ctx context.Context, ds *dataset.Dataset, loadClock clock.Clock,
	refs []CaseRef, truthByID map[string]dataset.ScenarioTruth) error {
	loadedAt := loadClock.Now().UTC()
	split := string(ds.Manifest.Split)

	if err := db.WithTx(ctx, s.Admin, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO eval_truth.sandbox_meta
			     (id, eval_dataset_version, split, seed, world_clock, loaded_at)
			 VALUES (1, $1, $2, $3, $4, $5)`,
			ds.Manifest.EvalDatasetVersion, split, ds.Manifest.Seed, loadedAt, loadedAt)
		return err
	}); err != nil {
		return fmt.Errorf("sandbox: write eval_truth.sandbox_meta: %w", err)
	}

	scenarios := append([]dataset.ScenarioTruth(nil), ds.Truth.Scenarios...)
	sort.Slice(scenarios, func(i, j int) bool {
		return scenarios[i].ScenarioID < scenarios[j].ScenarioID
	})
	for start := 0; start < len(scenarios); start += 100 {
		end := min(start+100, len(scenarios))
		batch := scenarios[start:end]
		if err := db.WithTx(ctx, s.Admin, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			for _, truth := range batch {
				document, err := json.Marshal(truth)
				if err != nil {
					return fmt.Errorf("marshal scenario %s: %w", truth.ScenarioID, err)
				}
				if _, err := tx.Exec(ctx,
					`INSERT INTO eval_truth.scenarios
					     (scenario_id, split, mode, difficulty, break_signature,
					      truth_schema_version, truth, loaded_at)
					 VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8)`,
					truth.ScenarioID, split, truth.Mode, truth.Difficulty,
					truth.BreakSignature, ds.Truth.SchemaVersion, document, loadedAt); err != nil {
					return fmt.Errorf("insert scenario %s: %w", truth.ScenarioID, err)
				}
			}
			return nil
		}); err != nil {
			return fmt.Errorf("sandbox: write eval_truth.scenarios: %w", err)
		}
	}

	for start := 0; start < len(refs); start += 100 {
		end := min(start+100, len(refs))
		batch := refs[start:end]
		if err := db.WithTx(ctx, s.Admin, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			for _, ref := range batch {
				var breakID any
				if ref.BreakID != "" {
					breakID = ref.BreakID
				}
				if _, err := tx.Exec(ctx,
					`INSERT INTO eval_truth.scenario_cases
					     (scenario_id, case_id, break_id, synthetic, scenario_time, created_at)
					 VALUES ($1, $2::uuid, $3::uuid, $4, $5, $6)`,
					ref.ScenarioID, ref.CaseID, breakID, ref.Synthetic,
					ref.ScenarioTime, loadedAt); err != nil {
					return fmt.Errorf("insert scenario case %s: %w", ref.ScenarioID, err)
				}
			}
			return nil
		}); err != nil {
			return fmt.Errorf("sandbox: write eval_truth.scenario_cases: %w", err)
		}
	}
	return nil
}
