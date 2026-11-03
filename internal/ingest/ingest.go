package ingest

import (
	"fmt"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/db"
)

// Audit actions and the entity type written by the ingestion pipeline
// (FIN-30). Every persisted batch gets exactly one entry: ingest.batch for an
// accepted file, ingest.quarantine for a rejected one.
const (
	ActionIngest     = "ingest.batch"
	ActionQuarantine = "ingest.quarantine"
	EntityBatch      = "settlement_batch"
)

// Status is a settlement batch's final ingestion status.
type Status string

// Batch statuses accepted by the settlement_batches CHECK constraint.
const (
	StatusIngested    Status = "ingested"
	StatusQuarantined Status = "quarantined"
)

// Options configures an Ingester.
type Options struct {
	// DB starts the ingestion transaction. *pgxpool.Pool satisfies it.
	DB db.Beginner
	// Clock supplies audit timestamps; domain code never calls time.Now.
	Clock clock.Clock
	// Actor is the typed audit actor (normally system:ingest).
	Actor audit.Actor
	// Sources is the per-source calendar configuration (FIN-41).
	Sources config.IngestConfig
}

// Ingester runs the ingestion pipeline: file-hash no-op, parse, FIN-50
// control totals, FIN-55 statement continuity, then one all-or-nothing
// transaction or a quarantined batch plus one audit entry.
type Ingester struct {
	db      db.Beginner
	clock   clock.Clock
	actor   audit.Actor
	sources Sources
}

// New validates opts and builds the ingester.
func New(opts Options) (*Ingester, error) {
	if opts.DB == nil {
		return nil, fmt.Errorf("%w: DB is required", ErrInvalidRequest)
	}
	if opts.Clock == nil {
		return nil, fmt.Errorf("%w: Clock is required", ErrInvalidRequest)
	}
	if !opts.Actor.Valid() {
		return nil, fmt.Errorf("%w: audit actor is not valid", ErrInvalidRequest)
	}
	sources, err := NewSources(opts.Sources)
	if err != nil {
		return nil, err
	}
	return &Ingester{
		db:      opts.DB,
		clock:   opts.Clock,
		actor:   opts.Actor,
		sources: sources,
	}, nil
}

// ValueDate exposes the per-source value-date derivation (FIN-41, FIN-42).
func (i *Ingester) ValueDate(source string, ts time.Time) (time.Time, error) {
	return i.sources.ValueDate(source, ts)
}

// Request is one file to ingest.
type Request struct {
	// Source is the configured source name (for example psp_a).
	Source string
	// FileName is for audit evidence only; it is never parsed.
	FileName string
	// Data is the complete file. The pipeline hashes exactly these bytes, so a
	// re-upload of identical bytes is a no-op even if FileName changes
	// (FIN-51).
	Data []byte
	// Parser turns Data into canonical records.
	Parser Parser
}

// Result reports what happened to one Ingest call. A quarantined batch is a
// successful pipeline outcome, not an error: Status and QuarantineReason carry
// the verdict. NoOp is true when the file hash was already seen (FIN-51).
type Result struct {
	// BatchID is the settlement_batches.id, also when quarantined.
	BatchID string
	// Status is ingested or quarantined.
	Status Status
	// NoOp is true when this exact file hash was already ingested or
	// quarantined; nothing was written and no audit entry was appended.
	NoOp bool
	// Parsed is the number of records the parser returned.
	Parsed int
	// Inserted is the number of external_records rows this call wrote. A
	// quarantined batch always inserts zero.
	Inserted int
	// Skipped is Parsed minus Inserted: duplicate (source, source_record_id)
	// records already present from an earlier file (FIN-51).
	Skipped int
	// QuarantineReason is non-empty exactly when Status is quarantined.
	QuarantineReason string
}
