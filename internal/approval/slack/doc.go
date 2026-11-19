// Package slack is the Slack approval channel adapter. It owns the Block Kit
// request message, the Slack Web API client, the approval_messages transport
// store, the approval.Channel implementation, the outbox handlers, and the
// signed interaction endpoint.
//
// Operator setup, app scopes, the interactivity URL, environment variables,
// the manual smoke checklist, and troubleshooting are documented in
// docs/slack-setup.md.
//
// Boundaries:
//
//   - Approval invariants stay in internal/approval. This package renders
//     messages, calls Slack, and calls the approval service; it does not write
//     approval SQL or decide whether a decision is allowed.
//   - Outbound delivery runs through the outbox. The chosen worker entry point
//     for M4-12 is a new minimal cmd/outbox binary, not cmd/worker, so outbox
//     deployments do not need agent or LLM credentials. The package must not
//     import cmd/*.
//   - FIN-22: never hold a database transaction open across a Slack network
//     call. The approval_messages store is single-statement; outbox handlers
//     hydrate and post outside the transaction.
//
// Secrets:
//
//   - The bot token and signing secret are environment-only config. This
//     package never logs, returns, or renders them. Client errors retain only
//     the HTTP status, Slack error code, attempt, and method; request and
//     response bodies are never echoed. Client, ClientOptions, Handlers,
//     InteractionHandler, and Channel implement String, GoString, and Format so
//     %v/%+v/%#v/%s render the bot token as <redacted>. ClientOptions (and
//     config.SlackConfig) also implement slog.LogValuer and json.Marshaler, so a
//     slog or encoding/json dump redacts the token and signing secret too.
//   - Block Kit text is rendered from untrusted database/agent data only after
//     ASCII control characters are stripped and &, <, > are escaped. URLs in
//     the message are server-built from configuration, never from row data.
//
// Channel compatibility: channel.go implements approval.Channel
// (RequestApproval, HandleCallback, Cancel) for interface compatibility and
// tests. Production delivery runs through Handlers and the outbox, not
// Channel.RequestApproval. RequestApproval performs short single-statement
// store calls around the Slack post and never opens a transaction across the
// network call (FIN-22).
//
// HandleCallback decodes an already-authenticated form body. Production
// authentication is the POST /v1/slack/interactions signature middleware, not
// HandleCallback itself; calling it directly does not verify a Slack signature
// and it never touches the replay cache. approval.CallbackDecision has no
// InfoRequested flag, so a request-info action is returned as
// DecisionRejected with Reason "info_requested"; a caller that needs the case
// lifecycle must use approval.Service.DecideWithOptions with
// DecisionOptions{InfoRequested: true}. This boundary is recorded in Q-97.
package slack
