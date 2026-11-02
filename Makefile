# Recon Agent Makefile — portable to GNU Make 3.81 and BSD make.
# Unimplemented targets print a message to stderr and exit 1; they never pass silently.

POSTGRES_PORT ?= 5432
DATABASE_URL ?= postgres://recon:recon@localhost:$(POSTGRES_PORT)/recon?sslmode=disable
TEST_DATABASE_URL ?= $(DATABASE_URL)
export DATABASE_URL
export TEST_DATABASE_URL

.PHONY: help build lint lint-negative test test-integration test-fault db-up db-down migrate test-db-clean verify fin-coverage must-not demo demo-m1 demo-m2

help:
	@echo "Targets:"
	@printf '  %-16s %s\n' 'help' 'list targets'
	@printf '  %-16s %s\n' 'build' 'go build ./...'
	@printf '  %-16s %s\n' 'lint' 'go vet ./... + golangci-lint v2.15.0'
	@printf '  %-16s %s\n' 'lint-negative' 'stub until M0-04'
	@printf '  %-16s %s\n' 'test' 'go test -race ./...'
	@printf '  %-16s %s\n' 'test-integration' 'stub until M0-02'
	@printf '  %-16s %s\n' 'test-fault' 'stub until M4-08'
	@printf '  %-16s %s\n' 'db-up' 'stub until M0-02'
	@printf '  %-16s %s\n' 'db-down' 'stub until M0-02'
	@printf '  %-16s %s\n' 'migrate' 'stub until M0-02'
	@printf '  %-16s %s\n' 'test-db-clean' 'stub until M0-02'
	@printf '  %-16s %s\n' 'verify' 'db-up migrate build lint test test-integration'
	@printf '  %-16s %s\n' 'fin-coverage' 'stub until M6-07'
	@printf '  %-16s %s\n' 'must-not' 'stub until M6-04'
	@printf '  %-16s %s\n' 'demo' 'stub until M5-12'
	@printf '  %-16s %s\n' 'demo-m1' 'stub until M1-16'
	@printf '  %-16s %s\n' 'demo-m2' 'stub until M2-11'

build:
	go build ./...

lint:
	go vet ./...
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0 run

lint-negative:
	@printf '%s\n' 'target lint-negative: not implemented until M0-04' >&2
	@exit 1

test:
	go test -race ./...

test-integration:
	@printf '%s\n' 'target test-integration: not implemented until M0-02' >&2
	@exit 1

test-fault:
	@printf '%s\n' 'target test-fault: not implemented until M4-08' >&2
	@exit 1

db-up:
	@printf '%s\n' 'target db-up: not implemented until M0-02' >&2
	@exit 1

db-down:
	@printf '%s\n' 'target db-down: not implemented until M0-02' >&2
	@exit 1

migrate:
	@printf '%s\n' 'target migrate: not implemented until M0-02' >&2
	@exit 1

test-db-clean:
	@printf '%s\n' 'target test-db-clean: not implemented until M0-02' >&2
	@exit 1

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
	@printf '%s\n' 'target demo-m1: not implemented until M1-16' >&2
	@exit 1

demo-m2:
	@printf '%s\n' 'target demo-m2: not implemented until M2-11' >&2
	@exit 1
