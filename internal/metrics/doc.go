// Package metrics owns the process-local Prometheus registry and the frozen
// reconciliation metric catalog.
//
// # Registry
//
// Every process constructs its own registry with New. The package never uses
// prometheus.DefaultRegisterer, the default global registry, or promauto, so
// tests and multiple APIs in one process cannot share counters by accident.
// The API, worker, and outbox long-running servers each pass one Metrics value
// to the collectors and producers they own.
//
// # Catalog
//
// The metric names, types, help text, label names, label allow-lists, and
// histogram buckets in catalog.go are a public contract (M5-04 consumes them).
// New validates the catalog before registering anything and fails startup on a
// duplicate name, a forbidden label, or an invalid specification.
//
// # Cardinality
//
// A label value is always bounded. Closed enums are checked against their
// allow-list; source, currency, model, and topic use validators. An invalid
// value is normalized to the literal "unknown" so it cannot create a new
// series. Identifier labels (case_id, proposal_id, trace_id, and any *_id) and
// free-text labels (payload, error, reason, url, dsn, email, and similar) are
// rejected at registration by RegisterChecked and by ValidateRegistry.
//
// # Seeded series
//
// Every vector is instantiated at construction with at least one child so an
// idle process still exposes every family. Labelled vectors are seeded with
// unknown values, except the three queue gauges which are seeded at 0 and the
// integrity gauge which is seeded as job="integrity" 0. Seeded values are part
// of the scrape output and deliberately make an absent producer visible. A
// seeded histogram child uses WithLabelValues and therefore has zero
// observations; the seed is not a fake sample.
//
// # Go, process, and promhttp exemptions
//
// Go, process, and promhttp collectors are owned by client_golang and expose
// their own families and label names. ValidateRegistry exempts the go_,
// process_, and promhttp_ prefixes from the business-cardinality allow-list.
// The process collector's available families depend on the host operating
// system. It is registered uniformly in long-running binaries, and the
// process_ prefix is exempt whether or not a platform emits process_* data.
//
// # Counter semantics
//
// Counters are process-local and reset when a process restarts. Dashboards must
// use PromQL rate/increase rather than raw totals. Gauges derived from shared
// database state are refreshed per scrape by the collectors that own them.
//
// # Database gauges
//
// Options.Queryer enables the scrape-time recon_queue_depth and
// recon_outbox_lag_seconds collectors against the process's recon_app pool.
// Each gather runs bounded, read-only aggregate queries under
// Options.GatherTimeout; a failed query keeps the gauge's last value, writes a
// bounded component/queue log line (never SQL, error text, or a DSN), and
// increments recon_errors_total{component="metrics",code="collect"} once per
// failed query in the same scrape. A nil Queryer keeps the static seeded gauges
// and issues no query.
//
// # HTTP listeners
//
// Handler serves one gatherer as Prometheus text with a bounded total-failure
// response. The API mounts it on its existing public GET /metrics route; the
// worker and outbox call StartForProcess on their configured loopback
// addresses only in long-running mode, so --once never races for the port.
package metrics
