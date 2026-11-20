# Slack approval setup

This page is the operator/developer guide for the M4-12 Slack approval
adapter: creating the Slack app, wiring the two runtime processes, seeding an
approver, running the manual smoke, and troubleshooting delivery.

Requirements: `docs/spec.md` section 8.3 and sections 10–12, and
`docs/ROADMAP.md` M4-12. The package boundary is documented in
`internal/approval/slack/doc.go`; the recorded residuals and accepted
boundaries are in `docs/open-questions.md` Q-97.

> **Status: code complete, manual smoke PENDING.** The AC-15 manual smoke has
> **not** been run in this repository/environment. M4-12 is not done until a
> human with a real Slack workspace and tunnel runs the checklist in
> [Manual smoke checklist](#manual-smoke-checklist-ac-15) and records the
> result. Do not tick the roadmap M4-12 box on the strength of the automated
> suite alone.

## What runs where

| Process | Slack responsibility | Required Slack configuration |
|---|---|---|
| `cmd/api` | Verifies the request signature and handles `POST /v1/slack/interactions` (approve, reject, request-info, denial/stale/ephemeral responses). | Bot token (for ephemerals) and signing secret. |
| `cmd/outbox` | Delivers `approval.request`, `approval.update`, and `approval.cancel` from the outbox to the Slack Web API. | Bot token, channel ID, and case-file base URL. |

The API never posts the approval request; the outbox worker does. The outbox
worker is a separate minimal binary, not a mode of `cmd/worker`, so an
outbox-only deployment does not need agent or LLM credentials (Q-97). Both
processes use the `recon_app` database role through `DATABASE_APP_URL` and
none of them starts or commits a database transaction around a Slack call
(FIN-22).

Message transport state lives in `approval_messages` with one row per
`(proposal_id, channel)` and exactly three states: `posted`, `updated`,
`cancelled`. The stored `external_ref` is the frozen form
`<channel_id>:<ts>` (for example `C0123456789:1712345678.123456`). It is not a
scheme URL and it is not written to `approvals.external_ref` (that field stays
for ticket adapters). See Q-97 for the at-least-once post/insert window.

## 1. Create the Slack app

1. Open <https://api.slack.com/apps> and choose **Create New App → From
   scratch**.
2. Pick the target workspace and name the app (for example
   `Recon Approvals`).
3. In **OAuth & Permissions → Scopes → Bot Token Scopes**, add exactly
   `chat:write`.
   - Prefer inviting the bot to the approvals channel. Only add
     `chat:write.public` if company policy prevents inviting the bot; that
     scope lets it post to channels it has not joined.
   - `chat.postEphemeral` uses the same `chat:write` scope. The bot must be a
     member of the channel (or have `chat:write.public` where applicable).
4. Choose **Install to Workspace**, review the permissions, and allow the
   install.
5. Copy the **Bot User OAuth Token** (`xoxb-…`) from **OAuth & Permissions**.
   This is `SLACK_BOT_TOKEN`. Treat it as a secret.
6. Open **Basic Information → App Credentials** and copy the **Signing
   Secret**. This is `SLACK_SIGNING_SECRET`. Treat it as a secret.

No other scopes are required by the current adapter. Do not enable incoming
webhooks, event subscriptions, slash commands, or a bot user presence for
M4-12.

## 2. Enable interactivity

1. In the app, open **Interactivity & Shortcuts** and turn interactivity on.
2. Set **Request URL** to:

   ```text
   https://<public-host>/v1/slack/interactions
   ```

   The path is exactly `/v1/slack/interactions`. The local API listens on
   `:8080` by default, so the tunnel target is `http://localhost:8080`.
3. Save. Interactivity does not use the Events API `url_verification`
   challenge flow; Slack delivers the first real `block_actions` payload when
   someone clicks a button. Use a real click to confirm the URL and signature
   wiring.

The API verifies Slack's `X-Slack-Signature` (`v0=<hex>`) and
`X-Slack-Request-Timestamp` over the raw request body with a five-minute
tolerance. It rejects an exact replay with `409`. Slack retries carry
`X-Slack-Retry-Num`; that header is **not** signed, so it only bypasses the
replay marker — the HMAC and timestamp checks are always enforced (Q-97).
Decision idempotency is the guard against a duplicated retry.

## 3. Invite the bot and get the channel ID

1. In Slack, open the channel that will receive approval requests and invite
   the app: `/invite @Recon Approvals` (or the app name you chose).
2. Find the channel ID: channel name → **About** → **Channel ID**, or use the
   Slack UI/API. Channel IDs look like `C0123456789`. This is
   `SLACK_CHANNEL_ID`.

The adapter posts only to this configured channel. A message about a different
channel, or a stored `external_ref` whose channel half does not match, is
treated as stale.

## 4. Environment variables

Set these in the process environment. Never commit real values; `.env.example`
contains empty placeholders only.

| Variable | `cmd/api` | `cmd/outbox` | Meaning |
|---|---|---|---|
| `DATABASE_APP_URL` | required | required | DSN for the `recon_app` role. |
| `SLACK_BOT_TOKEN` | required | required | `xoxb-…` bot token. Environment-only; a YAML secret is a strict-decode error. |
| `SLACK_SIGNING_SECRET` | required | — | App signing secret for interaction verification. Environment-only. |
| `SLACK_CHANNEL_ID` | — | required | Channel that receives approval requests. May also be set as `slack.channel_id` in `CONFIG_FILE`. |
| `SLACK_CASE_FILE_BASE_URL` | — | required | Absolute `http`/`https` base URL used to build the server-side case-file link. May also be set as `slack.case_file_base_url` in `CONFIG_FILE`. |
| `API_KEYS` | required | — | JSON array of `{id, role, key}` API principals; the decision route and all non-Slack API routes still need it. |
| `POLICY_FILE` | optional | — | Defaults to `policy.yaml`; the approval service needs the loaded policy. |
| `CONFIG_FILE` | optional | optional | YAML configuration path. |

`SLACK_CASE_FILE_BASE_URL` is the case-file base URL used in messages, for example
`https://recon.example.com`; the message links to
`<base>/v1/cases/<case-id>`. It must be an absolute URL without userinfo,
query, or fragment, and the trailing slash is normalized away. The link is
built server-side; row data never supplies a URL.

Local development example (the compose init script creates these
non-secret, laptop-only logins; see
`docs/decisions/0005-roles-and-test-provisioning.md`):

```sh
export DATABASE_APP_URL='postgres://recon_dev_app:recon_dev_app_local_only@localhost:5432/recon?sslmode=disable'
export SLACK_BOT_TOKEN='xoxb-…'                 # from your Slack app
export SLACK_SIGNING_SECRET='…'                 # from your Slack app
export SLACK_CHANNEL_ID='C0123456789'
export SLACK_CASE_FILE_BASE_URL='http://localhost:8080'
export API_KEYS='[{"id":"dev-approver","role":"approver","key":"<32+ random chars>"}]'
```

The API's required Slack startup validation is only the bot token and signing
secret. The outbox's required startup validation is the app DSN, bot token,
channel ID, and case-file base URL. Missing values fail startup with a clear
error that names the variable and never echoes the value.

Optional YAML shape (secrets are deliberately not accepted here):

```yaml
slack:
  channel_id: "C0123456789"
  case_file_base_url: "https://recon.example.com"
```

## 5. Seed an approver

The interaction handler maps a Slack user ID to an `approvers` row. There is
no runtime admin API for `approvers`; run this as the migration/owner role
(the `recon` superuser in local development), not as `recon_app`.

```sql
INSERT INTO approvers (display_name, role, slack_user_id, active)
VALUES ('Ada Approver', 'approver', 'U0123456789', true);
```

- `display_name` is shown in the updated Slack message.
- `role` must be `approver` or `admin`; `viewer` is denied at decision time.
- `slack_user_id` is the Slack member ID (`U…`), is unique, and must match the
  human clicking the button.
- `active` must be true. An inactive, unknown, or non-approver mapping
  returns `200` with an ephemeral denial and writes exactly one
  `approval.denied` audit row with actor `human:<slack user id>`; it changes no
  proposal, approval, or case row.
- `api_key_id` is optional for Slack. It is the static API-key mapping used by
  the non-Slack `POST /v1/proposals/{id}/decision` route (M4-11), and it is
  unrelated to the Slack user ID.
- To rotate or deactivate an approver, update/disable this row out of band.
  The application role cannot write `approvers`.

## 6. Run the stack and a tunnel

From the repository root:

```sh
make db-up
make migrate

# API: serves /v1/slack/interactions and everything else on :8080.
go run ./cmd/api

# Outbox: posts and updates Slack messages.
go run ./cmd/outbox
```

In a second terminal, expose the API with a development tunnel:

```sh
# Cloudflare Tunnel (no account needed for a quick tunnel):
cloudflared tunnel --url http://localhost:8080

# or ngrok:
ngrok http 8080
```

Copy the public `https://…` host into **Interactivity & Shortcuts → Request
URL** as `https://<public-host>/v1/slack/interactions`. Quick tunnels rotate
their hostname; update the Slack request URL after every restart.

Health check before configuring Slack:

```sh
curl -sS http://localhost:8080/healthz
```

Useful local checks while the smoke is running:

```sh
# Outbox rows (admin DSN; this is an inspection query, not an app write).
docker compose exec -T postgres psql -U recon -d recon -c \
  "SELECT topic, attempts, delivered_at, dead FROM outbox ORDER BY created_at DESC LIMIT 10;"

# Transport rows; external_ref must be <channel_id>:<ts>.
docker compose exec -T postgres psql -U recon -d recon -c \
  "SELECT proposal_id, channel, external_ref, state FROM approval_messages;"

# Audit trail, including approval.denied and approval.decided.
docker compose exec -T postgres psql -U recon -d recon -c \
  "SELECT seq, actor_id, action, entity_type, entity_id FROM audit_log ORDER BY seq DESC LIMIT 20;"
```

`cmd/outbox --once` processes at most one due batch and exits; without
`--once` it polls until SIGINT/SIGTERM.

## 7. Producing a pending T1 proposal for the smoke

M4-12 starts from a proposal in `pending_approval` with tier T1 and an
enqueued `approval.request` outbox row. **This release does not add a
production demo/CLI trigger that creates a T1 proposal and calls
`approval.Service.Request`.** M4-14 owns the end-to-end pipeline that wires
agent output through policy, approval request, and the simulated approver.
Until that wiring is deployed, run the manual smoke with one of:

- the M4-14 end-to-end harness once available; or
- a reviewed local harness/fixture that calls `approval.Service.Request` for
  a real `proposed` T1 proposal.

Do not hand-edit `proposals.status` or insert `approval_messages` rows to fake
the flow; the smoke must exercise the real `Request` → outbox → Slack path.
For reference, find a pending proposal with:

```sql
SELECT p.id, p.status, p.risk_tier, p.case_id
FROM proposals p
WHERE p.status = 'pending_approval'
ORDER BY p.created_at DESC;
```

## Manual smoke checklist (AC-15)

> **PENDING — HUMAN-GATED. NOT RUN.** This checklist cannot pass in this
> environment. A human with a real Slack workspace and a public tunnel must
> run it once and record the result below. Automated tests, fake Slack
> servers, and sanitized fixtures do not satisfy AC-15.

### Known residuals before AC-15

The final adversary review at `a9ea9f1` recorded four non-blocking residuals in
`docs/open-questions.md` Q-97 (F1–F4). They do not change the smoke steps, but
read them before recording a result:

- **F1 (high, pre-existing/out-of-scope):** whole-`Config` JSON/slog redacts the
  Slack bot token and signing secret but still renders database DSNs/passwords
  and the LLM API key. Do not JSON-marshal or `slog.Any` a whole `Config`
  during the smoke; log `cfg.Slack` or use `%v`/`String`, which redact
  everything. Tracked as separate config hardening (propose FIN-62 if adopted
  broadly), not an M4-12 failure.
- **F2 (medium):** a decision that lands between the Slack post and the
  `approval_messages` insert can consume `approval.update` and leave a decided
  message with live buttons. Clicks are still refused by the proposal
  state/version check, so this is a stale-message gap, not a wrong decision.
- **F3 (low):** `PostMessageRef` trusts Slack's response channel; a malformed
  channel value could produce an unparseable `external_ref`. Real channel IDs
  cannot contain `:`, so this is robustness only.
- **F4 (medium, AC-15 risk):** the strict nested `message` allow-list rejects
  message fields it does not know. If a real workspace payload carries
  `app_id`, `files`, `bot_profile`, a nested `team`, or similar, the click
  returns `400 invalid_request`. If the smoke reproduces this, stop and record
  it: AC-15 fails, and the fix is to relax `message` to `json.RawMessage` and
  validate only `message.ts` against the stored external ref.

Preconditions: Postgres migrated; API and outbox running; tunnel host set as
the Slack interactivity request URL; bot invited to the channel; one active
`approver`/`admin` seeded with the operator's real `slack_user_id`; a pending
T1 proposal exists (section 7).

1. Start Postgres, run migrations, start `cmd/api` and `cmd/outbox`, start the
   tunnel, and set the Slack interactivity request URL. Confirm `/healthz`
   returns 200 and the tunnel host is reachable.
2. Confirm the seeded approver row has the operator's real `slack_user_id`,
   role `approver` or `admin`, and `active = true`.
3. Trigger the T1 proposal and confirm the message renders: case summary,
   delta, evidence, verifier result, rationale, T1 badge, case-file link, and
   the three buttons **Approve**, **Reject**, **Request info**. Confirm the
   message is in the configured channel and `approval_messages.state` is
   `posted`.
4. Click **Approve**. Confirm the ephemeral acknowledgement, the message
   updates in place with decision/approver/time and no buttons
   (`approval_messages.state = 'updated'`), the proposal is `approved`, and the
   case advances per lifecycle.
5. Trigger a second T1 proposal, click **Reject**, and confirm the in-place
   update, a proposal `rejected` transition, and an `approvals` row plus
   `approval.decided` audit.
6. Trigger a third T1 proposal, click **Request info**, and confirm the
   proposal is `rejected` with transition reason `info_requested`, the case is
   `waiting_info`, `cases.attempts` is unchanged, the message is updated, and
   the `approval.decided` audit has `info_requested: true`.
7. Click a button from a Slack user with no active approver mapping. Confirm
   the ephemeral denial, exactly one `approval.denied` audit with actor
   `human:<slack user id>` and `slack_user_id` in the payload, and no
   proposal/case/approvals change.
8. Click an old/stale message (or a button whose proposal is no longer
   pending/whose version no longer matches). Confirm the ephemeral
   "This approval is no longer active." and no `approvals` row. A
   stale/not-pending/mismatched click changes no proposal, case, or approval
   row. **Expired pending proposal:** if only `expires_at` has passed and the
   proposal is still `pending_approval`, the click commits
   `pending_approval -> expired` (one `proposal.transition` audit, actor
   `system:expiry`, reason `expired`) and runs the case lifecycle hook; it
   still writes no `approvals` row and no `approval.decided` audit, and no
   `approval.update` is enqueued, so the posted message may keep its buttons
   (Q-97(4)).
9. Record the date, workspace, operator, API/outbox commit SHA, result, and
   any deviations in the table below.

### Smoke results

| Date | Slack workspace | Operator | API/outbox commit | Result | Notes/deviations |
|---|---|---|---|---|---|
| — | — | — | — | **PENDING — not run** | Human-gated; do not fabricate. |

## Troubleshooting

| Symptom | Likely cause | What to do |
|---|---|---|
| Startup fails with `slack.bot_token is required` / `slack.signing_secret is required` | Env var missing in that process. | Set `SLACK_BOT_TOKEN` in API **and** outbox; set `SLACK_SIGNING_SECRET` in the API. |
| Startup fails with `slack.channel_id is required` / `slack.case_file_base_url is required` | Outbox-only vars missing. | Set `SLACK_CHANNEL_ID` and `SLACK_CASE_FILE_BASE_URL` (or the YAML equivalents). |
| `invalid_auth` in outbox logs | Bot token revoked, mistyped, or from another workspace/app. | Reinstall the app and replace `SLACK_BOT_TOKEN`. Do not paste the token into a ticket or doc. |
| `not_in_channel` | Bot not invited to the configured channel. | `/invite @YourApp` in the channel, or (only if necessary) add `chat:write.public`. |
| `channel_not_found` | Wrong `SLACK_CHANNEL_ID`, deleted/archived channel, or the bot cannot see a private channel. | Copy the channel ID again and invite the bot. |
| HTTP 429 / `ratelimited` with `Retry-After` | Slack rate limit. | The client retries within its 2-retry/10 s budget. Persistent 429 means reduce delivery volume or split channels; check outbox attempts/backoff. |
| 401 `unauthenticated` (signature verification failed) | Wrong signing secret, clock skew beyond five minutes, a proxy rewriting the form body, or a stale tunnel URL. | Replace the signing secret, sync the host clock, ensure the proxy forwards the raw body unchanged, and update the interactivity URL. |
| 409 `replay` | The same signed request was delivered twice without `X-Slack-Retry-Num`. | Expected replay protection. A genuine Slack retry carries the retry header and is processed; duplicate decisions are idempotent. |
| `400 invalid_request` on a real click | The payload carries a message field outside the strict nested allow-list (Q-97 F4), or a genuinely malformed/stale action. | Capture the redacted payload and record it; AC-15 fails until the message allow-list is relaxed/validated. Do not widen the allow-list ad hoc during the smoke. |
| 413 `payload_too_large` | Interaction payload above the 1 MiB verifier cap. | Check for a malformed client/proxy; normal Block Kit actions are far below the cap. |
| Slack retries a click repeatedly (500) | Internal failure before the decision commits, or an audit failure. | Check API logs by trace ID; fix the dependency. Slack retries are safe because decisions are idempotent. |
| No message appears but `approval.request` row exists | Outbox not running, provider error, T2/`ticket` row skipped, or proposal left pending before delivery. | Run `cmd/outbox`, inspect attempts/dead, and confirm the proposal is still `pending_approval` and `channel = 'slack'`. |
| Message not updated after a decision | The decision happened before Slack was reachable; `approval.update` may have no matching `approval_messages` row, or a click on an already-expired pending proposal recorded the expiry without enqueuing an update. | Inspect outbox and `approval_messages`. A late click on an expired proposal still commits `pending_approval -> expired` and the lifecycle hook, so check the proposal status and audit before retrying. See Q-97(4). |
| `message_not_found` / `cant_update_message` | Message deleted or no longer editable. | The handler treats it as terminal and marks the transport row `cancelled`; no retry will succeed. |
| `approval.denied` audit but no proposal change | Unknown, inactive, or `viewer` Slack identity. | Fix the `approvers` row; denials are deliberate and audited. |
| T2 rows are skipped | M4-12 has no ticket adapter; the request handler acknowledges `channel="ticket"` as a no-op. | Expected until the ticket adapter (later milestone). |

## Security

- **Least privilege.** `chat:write` only. Prefer inviting the bot to one
  channel over `chat:write.public`.
- **Secrets are environment-only.** The bot token and signing secret are not
  accepted in YAML, are never logged, and are redacted by config formatting
  and by the JSON/slog renderers. Never paste them into this document, an
  issue, a chat, or a screenshot.
- **Do not JSON-log a whole `Config`.** `SlackConfig` redacts the bot token
  and signing secret in JSON/slog, but a whole `Config` value still renders
  database DSNs/passwords and the LLM API key (Q-97 F1). Log `cfg.Slack`, or
  use `%v`/`String`, which redact the whole configuration.
- **Rotate after exposure.** Reinstall the app / regenerate the signing
  secret and update the process environment; rotate one process at a time.
- **Do not log Slack payloads.** The adapter logs only method, status, error
  code, attempt, and trace ID. Slack payloads can contain case data and user
  IDs.
- **Tunnel hygiene.** Use a short-lived development tunnel; anyone with the
  URL can reach the API. It still requires a valid signature for the Slack
  route and API keys elsewhere, but do not expose an unauthenticated dev
  instance beyond the smoke window.
- **Do not bypass the signature.** The route middleware is the production
  crypto boundary. `Channel.HandleCallback` parses an already-authenticated
  body only; calling it directly does not verify Slack (Q-97).

## References

- `docs/spec.md` section 8.3 (Slack adapter), section 10 (documentation),
  section 11 (residuals), section 12 / 12.1 (acceptance and manual smoke).
- `docs/ROADMAP.md` M4-12.
- `docs/open-questions.md` Q-97 (M4-12 residuals).
- `internal/approval/slack/doc.go` (package boundary).
