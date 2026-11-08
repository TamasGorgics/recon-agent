# Recon Agent Makefile — portable to GNU Make 3.81 and BSD make.
# Unimplemented targets print a message to stderr and exit 1; they never pass silently.

POSTGRES_PORT ?= 5432
DATABASE_URL ?= postgres://recon:recon@localhost:$(POSTGRES_PORT)/recon?sslmode=disable
TEST_DATABASE_URL ?= $(DATABASE_URL)
export DATABASE_URL := $(DATABASE_URL)
export TEST_DATABASE_URL := $(TEST_DATABASE_URL)

.PHONY: help build lint lint-negative test test-integration test-live test-fault db-up db-down migrate test-db-clean verify fin-coverage must-not demo demo-m1 demo-m2

help:
	@echo "Targets:"
	@printf '  %-16s %s\n' 'help' 'list targets'
	@printf '  %-16s %s\n' 'build' 'go build ./...'
	@printf '  %-16s %s\n' 'lint' 'go vet + floatlint + migrationlint + golangci-lint v2.15.0 + scripts/lint-negative.sh'
	@printf '  %-16s %s\n' 'lint-negative' 'scripts/lint-negative.sh'
	@printf '  %-16s %s\n' 'test' 'go test -race ./...'
	@printf '  %-16s %s\n' 'test-integration' 'go test -race -tags=integration -parallel=4 -p 1 ./...'
	@printf '  %-16s %s\n' 'test-live' 'manual live LLM test; requires OPENCODE_API_KEY'
	@printf '  %-16s %s\n' 'test-fault' 'stub until M4-08'
	@printf '  %-16s %s\n' 'db-up' 'docker compose up -d --wait postgres'
	@printf '  %-16s %s\n' 'db-down' 'docker compose down'
	@printf '  %-16s %s\n' 'migrate' 'go run ./cmd/recon migrate up'
	@printf '  %-16s %s\n' 'test-db-clean' 'go run ./cmd/recon testdb clean'
	@printf '  %-16s %s\n' 'verify' 'db-up migrate build lint test test-integration'
	@printf '  %-16s %s\n' 'fin-coverage' 'stub until M6-07'
	@printf '  %-16s %s\n' 'must-not' 'stub until M6-04'
	@printf '  %-16s %s\n' 'demo' 'stub until M5-12'
	@printf '  %-16s %s\n' 'demo-m1' 'M1 end-to-end demo: seed 1, realistic, 14 days'
	@printf '  %-16s %s\n' 'demo-m2' 'M2 end-to-end gate: replay cassettes, no key required'

build:
	go build ./...

lint:
	go vet ./...
	go run ./tools/floatlint ./...
	go run ./tools/migrationlint ./migrations
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0 run
	./scripts/lint-negative.sh

lint-negative:
	./scripts/lint-negative.sh

test:
	go test -race ./...

# -p 1: eval/sandbox's CleanupOrphans (by design) drops every recon_eval_% clone,
# including a live clone owned by the concurrently running eval/runner test
# process. Serialize package binaries so destructive cleanup can only touch
# leftovers; -parallel=4 still parallelizes tests within a package.
test-integration:
	go test -race -tags=integration -parallel=4 -p 1 ./...

test-live:
	go test -tags=live -run 'TestLive|TestRecordM2Cassettes' -v -count=1 ./internal/llm ./internal/agent

test-fault:
	@printf '%s\n' 'target test-fault: not implemented until M4-08' >&2
	@exit 1

db-up:
	docker compose up -d --wait postgres

db-down:
	docker compose down

migrate:
	go run ./cmd/recon migrate up

test-db-clean:
	go run ./cmd/recon testdb clean

verify: db-up migrate build lint test test-integration
	@echo "verify: OK"

fin-coverage:
	@printf '%s\n' 'target fin-coverage: not implemented until M6-07' >&2
	@exit 1

must-not:
	@printf '%s\n' 'target must-not: not implemented until M6-04' >&2
	@exit 1

demo:
	@printf '%s\n' 'target demo: not implemented until M5-12' >&2
	@exit 1

demo-m1:
	go run ./cmd/recon demo seed --seed 1 --days 14 --difficulty realistic --reset

demo-m2:
	go test -race -tags=integration -run TestMilestone2Gate -v -count=1 ./internal/agent
