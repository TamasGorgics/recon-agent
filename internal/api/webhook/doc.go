// Package webhook implements reusable inbound webhook signature verification:
// one HMAC-SHA256 signature header, one Unix-seconds timestamp header, a
// bounded timestamp tolerance, a capped and restored body, and an in-memory
// replay cache.
//
// The default canonical string is Slack-compatible:
//
//	v0:<timestamp>:<raw body>
//
// where "v0" is the configured signature prefix without its trailing "=". The
// signature header is <prefix><hex SHA-256 HMAC>; hex may be upper- or
// lower-case. Replay detection decodes the hex and keys on the MAC bytes, so
// case variants of the same signature are one replay marker. Timestamps older
// or newer than the tolerance (5m by default) are rejected. Exact replays
// inside the replay TTL are rejected with 409 replay.
//
// The middleware never logs the secret, signature, timestamp, or body. It logs
// only a rejection reason, request ID, and trace ID. The replay cache is
// process-local and lazily pruned; it deliberately has no background janitor.
// A full cache fails closed rather than evicting a live replay marker.
//
// The package does not import internal/api; callers pass internal/api's error
// writer through Options.WriteError so the public error envelope is identical.
package webhook
