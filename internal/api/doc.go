// Package api implements the HTTP surface of the reconciliation service: the
// chi router, request-ID/trace middleware, bearer-key authentication and role
// checks, strict JSON and multipart decoding, the case/proposal read model, and
// the audited switch write service.
//
// Handlers depend on small interfaces (IngestService, MatchService,
// CaseService, readers, SwitchService) so they can be tested without a
// database. cmd/api wires the real ingest, match, cases, and Postgres
// implementations through Options.
package api
