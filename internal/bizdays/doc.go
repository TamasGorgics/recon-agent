// Package bizdays owns the shared FIN-41 business-day calendar: per-source
// timezone, settlement cutoff, business weekdays, and holidays.
//
// The arithmetic was extracted from internal/ingest with no semantic change so
// the ingest value-date path and the case missing-settlement SLA use exactly
// one calendar implementation. internal/ingest delegates here; callers that
// only need value dates can keep using the ingest facade.
package bizdays
