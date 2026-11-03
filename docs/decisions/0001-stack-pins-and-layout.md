# 0001 — Stack pins and repository layout

- **Status:** Accepted
- **Date:** 2026-11-02

## Context

M0-01 creates the module and skeleton for the Payment Reconciliation Agent. The
approved roadmap pins the stack, and the product spec §17 layout predates a few
roadmap packages. This record fixes the pins and the layout delta so later tasks
do not re-litigate them.

## Decision

- **Module path:** `github.com/TamasGorgics/recon-agent` (single module).
- **Go:** `go 1.27.3` in `go.mod`; the effective toolchain is go1.27.3. The
  installed probe was `go1.27.1 darwin/arm64`; `GOTOOLCHAIN=go1.27.3`
  auto-downloaded and ran `go version go1.27.3 darwin/arm64`. Go 1.27.3's
  `go mod tidy` removes a `toolchain go1.27.3` line as redundant with the
  `go 1.27.3` directive, and `go build`/`go vet` fail with "updates to go.mod
  needed" while the redundant line is present. The canonical `go.mod` therefore
  pins `go 1.27.3` only; `go env GOVERSION` reports `go1.27.3`. The HD-02
  fallback (`1.26.10`) was not needed and is not used.
- **golangci-lint:** v2 config schema (`version: "2"`), standard default linter
  set, pinned at v2.15.0 via `go run
  github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0 run`.
- **pgx:** `github.com/jackc/pgx/v5` v5.12.0 (added in M0-02).
- **goose:** `github.com/pressly/goose/v3` v3.29.0 (added in M0-02).
- **Postgres:** pin the newest available `postgres:16.<minor>` at M0-02, with
  the identical tag in `compose.yaml` and CI. No floating `16` and no `latest`.
- **Compose location:** root `compose.yaml`, a deliberate exception to spec §17
  so plain `docker compose up` works.
- **Docs path:** `docs/spec.md` and `docs/ROADMAP.md` are canonical. Do not
  create root copies; `AGENTS.md` is a short pointer to them.
- **Layout delta (spec §17):** the roadmap adds
  `internal/{money,db,testdb,clock,config,outbox,periods,tieout,matchscore,fault,worker}`
  plus the root `compose.yaml`. `deploy/` stays reserved for Grafana/Prometheus
  provisioning at M5-04.

## Consequences

- All Go commands honor the module's `go 1.27.3` requirement (effective toolchain
  go1.27.3).
- Later tasks may add dependencies but must keep these pins unless a new ADR
  supersedes them.
- Package directories that exist only in the delta are created when their owning
  milestone needs them, not at M0.
