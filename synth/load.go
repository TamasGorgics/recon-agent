package synth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/ingest"
	"github.com/TamasGorgics/recon-agent/internal/ledger"
)

// ErrInvalidLoad reports a load request that cannot be attempted.
var ErrInvalidLoad = errors.New("synth: invalid load request")

// LoadOptions configures Load. DB, Clock, and Actor are required. Sources may
// be empty; the generator's default psp_a/bank_x calendars are then used for
// any source the caller did not configure.
type LoadOptions struct {
	Dir     string
	DB      db.Beginner
	Clock   clock.Clock
	Actor   audit.Actor
	Sources config.IngestConfig
	Ledger  config.LedgerConfig
}

// LoadResult reports what one load did. A quarantined batch is a load outcome,
// not an error: the clean M1-09 dataset must produce zero.
type LoadResult struct {
	Accounts          int
	FeeSchedules      int
	Scopes            int
	LedgerPosted      int
	LedgerReplayed    int
	Files             int
	NoOpFiles         int
	RecordsInserted   int
	Quarantined       int
	QuarantineReasons []string
}

// Load writes one generated directory into Postgres:
//
//	accounts + fee_schedules + reconciliation_scopes -> ledger.Post -> ingest.
//
// Every step is idempotent on the same directory: account codes and fee
// schedule IDs are stable, ledger transactions replay by idempotency key, and
// ingestion no-ops on identical file hashes (FIN-51).
func Load(ctx context.Context, opts LoadOptions) (LoadResult, error) {
	if strings.TrimSpace(opts.Dir) == "" {
		return LoadResult{}, fmt.Errorf("%w: directory is required", ErrInvalidLoad)
	}
	if opts.DB == nil {
		return LoadResult{}, fmt.Errorf("%w: DB is required", ErrInvalidLoad)
	}
	if opts.Clock == nil {
		return LoadResult{}, fmt.Errorf("%w: clock is required", ErrInvalidLoad)
	}
	if !opts.Actor.Valid() {
		return LoadResult{}, fmt.Errorf("%w: audit actor is invalid", ErrInvalidLoad)
	}

	document, err := readLedgerDocument(opts.Dir)
	if err != nil {
		return LoadResult{}, err
	}
	result := LoadResult{
		Accounts:     len(document.Accounts),
		FeeSchedules: len(document.FeeSchedules),
		Scopes:       len(document.ReconciliationScopes),
	}

	accountIDs, err := loadReferenceData(ctx, opts.DB, document)
	if err != nil {
		return LoadResult{}, err
	}
	posted, replayed, err := loadTransactions(ctx, opts, document, accountIDs)
	if err != nil {
		return LoadResult{}, err
	}
	result.LedgerPosted = posted
	result.LedgerReplayed = replayed

	if err := loadFiles(ctx, opts, &result); err != nil {
		return LoadResult{}, err
	}
	sort.Strings(result.QuarantineReasons)
	return result, nil
}

func readLedgerDocument(dir string) (LedgerDocument, error) {
	data, err := os.ReadFile(filepath.Join(dir, "ledger.json"))
	if err != nil {
		return LedgerDocument{}, fmt.Errorf("synth: read ledger.json: %w", err)
	}
	var document LedgerDocument
	if err := json.Unmarshal(data, &document); err != nil {
		return LedgerDocument{}, fmt.Errorf("synth: parse ledger.json: %w", err)
	}
	if document.DatasetVersion == "" {
		return LedgerDocument{}, fmt.Errorf("%w: ledger.json has no dataset_version", ErrInvalidLoad)
	}
	if len(document.Transactions) == 0 {
		return LedgerDocument{}, fmt.Errorf("%w: ledger.json has no transactions", ErrInvalidLoad)
	}
	return document, nil
}

// loadReferenceData upserts accounts, fee schedules, and scopes in one
// transaction and returns the database account IDs keyed by account code.
//
// Existing rows win over generated IDs so a re-load against a database whose
// accounts were created by an earlier dataset (or by M1-16's demo bootstrap)
// still posts against the real account UUIDs.
func loadReferenceData(ctx context.Context, beginner db.Beginner, document LedgerDocument) (map[string]string, error) {
	accountIDs := make(map[string]string, len(document.Accounts))
	err := db.WithTx(ctx, beginner, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		for _, account := range document.Accounts {
			if _, err := tx.Exec(ctx,
				`INSERT INTO accounts (id, code, name, type, currency)
				 VALUES ($1::uuid, $2, $3, $4, $5)
				 ON CONFLICT (code) DO NOTHING`,
				account.ID, account.Code, account.Name, account.Type, account.Currency); err != nil {
				return fmt.Errorf("synth: insert account %s: %w", account.Code, err)
			}
		}
		codes := make([]string, 0, len(document.Accounts))
		for _, account := range document.Accounts {
			codes = append(codes, account.Code)
		}
		rows, err := tx.Query(ctx,
			`SELECT code, id::text FROM accounts WHERE code = ANY($1::text[])`, codes)
		if err != nil {
			return fmt.Errorf("synth: read account ids: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var code, id string
			if err := rows.Scan(&code, &id); err != nil {
				return fmt.Errorf("synth: scan account id: %w", err)
			}
			accountIDs[code] = id
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("synth: iterate account ids: %w", err)
		}

		for _, schedule := range document.FeeSchedules {
			var validTo any
			if schedule.ValidTo != nil {
				validTo = *schedule.ValidTo
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO fee_schedules
				     (id, source, kind, currency, pct_bps, fixed_minor, valid_from, valid_to)
				 VALUES ($1::uuid, $2, $3, $4, $5, $6, $7::date, $8::date)
				 ON CONFLICT (id) DO UPDATE SET
				     source = EXCLUDED.source,
				     kind = EXCLUDED.kind,
				     currency = EXCLUDED.currency,
				     pct_bps = EXCLUDED.pct_bps,
				     fixed_minor = EXCLUDED.fixed_minor,
				     valid_from = EXCLUDED.valid_from,
				     valid_to = EXCLUDED.valid_to`,
				schedule.ID, schedule.Source, schedule.Kind, schedule.Currency,
				schedule.PctBps, schedule.FixedMinor, schedule.ValidFrom, validTo); err != nil {
				return fmt.Errorf("synth: insert fee schedule %s: %w", schedule.ID, err)
			}
		}

		for _, scope := range document.ReconciliationScopes {
			accountID, ok := accountIDs[scope.LedgerAccountCode]
			if !ok {
				return fmt.Errorf("synth: scope %s/%s references unknown account %q",
					scope.Source, scope.Currency, scope.LedgerAccountCode)
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO reconciliation_scopes (ledger_account_id, source, currency)
				 VALUES ($1::uuid, $2, $3)
				 ON CONFLICT (source, currency) DO UPDATE
				 SET ledger_account_id = EXCLUDED.ledger_account_id`,
				accountID, scope.Source, scope.Currency); err != nil {
				return fmt.Errorf("synth: insert scope %s/%s: %w", scope.Source, scope.Currency, err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return accountIDs, nil
}

func loadTransactions(ctx context.Context, opts LoadOptions, document LedgerDocument,
	accountIDs map[string]string) (posted, replayed int, err error) {
	service := ledger.NewService(opts.Clock, opts.Ledger)
	err = db.WithTx(ctx, opts.DB, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		for _, transaction := range document.Transactions {
			request, err := postRequest(transaction, accountIDs, opts.Actor)
			if err != nil {
				return err
			}
			result, err := service.Post(ctx, tx, request)
			if err != nil {
				return fmt.Errorf("synth: post transaction %s: %w", transaction.ID, err)
			}
			if result.Replayed {
				replayed++
			} else {
				posted++
			}
		}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return posted, replayed, nil
}

func postRequest(transaction Transaction, accountIDs map[string]string, actor audit.Actor) (ledger.PostRequest, error) {
	valueDate, err := time.ParseInLocation(dateLayout, transaction.ValueDate, time.UTC)
	if err != nil {
		return ledger.PostRequest{}, fmt.Errorf("synth: transaction %s value_date: %w", transaction.ID, err)
	}
	createdAt, err := time.Parse(time.RFC3339, transaction.CreatedAt)
	if err != nil {
		return ledger.PostRequest{}, fmt.Errorf("synth: transaction %s created_at: %w", transaction.ID, err)
	}
	bookedAt, err := time.Parse(time.RFC3339, transaction.BookedAt)
	if err != nil {
		return ledger.PostRequest{}, fmt.Errorf("synth: transaction %s booked_at: %w", transaction.ID, err)
	}
	lines := make([]ledger.Line, 0, len(transaction.Lines))
	for _, line := range transaction.Lines {
		accountID, ok := accountIDs[line.AccountCode]
		if !ok {
			return ledger.PostRequest{}, fmt.Errorf("synth: transaction %s references unknown account %q",
				transaction.ID, line.AccountCode)
		}
		lines = append(lines, ledger.Line{
			AccountID:   accountID,
			Direction:   ledger.Direction(line.Direction),
			AmountMinor: line.AmountMinor,
			Currency:    line.Currency,
			ExternalRef: line.ExternalRef,
			Metadata:    line.Metadata,
		})
	}
	return ledger.PostRequest{
		IdempotencyKey: transaction.IdempotencyKey,
		Kind:           ledger.Kind(transaction.Kind),
		ValueDate:      valueDate,
		Description:    transaction.Description,
		Lines:          lines,
		Actor:          actor,
		ID:             transaction.ID,
		CreatedAt:      createdAt,
		BookedAt:       bookedAt,
	}, nil
}

func loadFiles(ctx context.Context, opts LoadOptions, result *LoadResult) error {
	sources := effectiveSources(opts.Sources)
	ingester, err := ingest.New(ingest.Options{
		DB:      opts.DB,
		Clock:   opts.Clock,
		Actor:   opts.Actor,
		Sources: sources,
	})
	if err != nil {
		return fmt.Errorf("synth: create ingester: %w", err)
	}

	files, err := sourceFiles(opts.Dir)
	if err != nil {
		return err
	}
	parsers := map[string]ingest.Parser{
		SourcePSP:  ingest.NewPSPParser(),
		SourceBank: ingest.NewBankParser(),
	}
	for _, file := range files {
		parser := parsers[file.Source]
		ingested, err := ingester.Ingest(ctx, ingest.Request{
			Source:   file.Source,
			FileName: file.Name,
			Data:     file.Data,
			Parser:   parser,
		})
		if err != nil {
			return fmt.Errorf("synth: ingest %s/%s: %w", file.Source, file.Name, err)
		}
		result.Files++
		if ingested.NoOp {
			result.NoOpFiles++
			continue
		}
		result.RecordsInserted += ingested.Inserted
		if ingested.Status == ingest.StatusQuarantined {
			result.Quarantined++
			result.QuarantineReasons = append(result.QuarantineReasons,
				fmt.Sprintf("%s/%s: %s", file.Source, file.Name, ingested.QuarantineReason))
		}
	}
	return nil
}

func effectiveSources(configured config.IngestConfig) config.IngestConfig {
	sources := make(map[string]config.SourceConfig, len(configured.Sources)+2)
	for name, source := range configured.Sources {
		sources[name] = source
	}
	for name, source := range config.DefaultSynthIngestSources() {
		if _, ok := sources[name]; !ok {
			sources[name] = source
		}
	}
	return config.IngestConfig{Sources: sources}
}

type sourceFile struct {
	Source string
	Name   string
	Data   []byte
}

func sourceFiles(dir string) ([]sourceFile, error) {
	var files []sourceFile
	for _, source := range []string{SourcePSP, SourceBank} {
		root := filepath.Join(dir, source)
		entries, err := os.ReadDir(root)
		if err != nil {
			return nil, fmt.Errorf("synth: read %s directory: %w", source, err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".csv") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(root, entry.Name()))
			if err != nil {
				return nil, fmt.Errorf("synth: read %s/%s: %w", source, entry.Name(), err)
			}
			files = append(files, sourceFile{Source: source, Name: entry.Name(), Data: data})
		}
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].Source != files[j].Source {
			return files[i].Source < files[j].Source
		}
		return files[i].Name < files[j].Name
	})
	return files, nil
}
