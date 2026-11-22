#!/bin/sh
# Emit one scripted agent trace to the local Phoenix and verify receipt through
# the Phoenix REST API (evidence level 2). UI visibility is level 3 and remains
# a manual human check; this script never claims it.
#
# Usage:
#   scripts/phoenix-demo.sh
#
# Environment overrides:
#   PHOENIX_URL           API/UI base URL       (default http://localhost:6006)
#   PHOENIX_OTLP_ENDPOINT OTLP/HTTP endpoint    (default http://localhost:4318)
#   TEST_DATABASE_URL     admin DSN for the ephemeral test DB
#                         (default postgres://recon:recon@localhost:5432/recon?sslmode=disable)
#
# Requires Docker Compose and a reachable Postgres (the script runs db-up).
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"

PHOENIX_URL=${PHOENIX_URL:-http://localhost:6006}
PHOENIX_OTLP_ENDPOINT=${PHOENIX_OTLP_ENDPOINT:-http://localhost:4318}
TEST_DATABASE_URL=${TEST_DATABASE_URL:-postgres://recon:recon@localhost:5432/recon?sslmode=disable}
export TEST_DATABASE_URL

printf 'phoenix-demo: checking compose configuration\n'
docker compose config -q

printf 'phoenix-demo: starting postgres and phoenix\n'
make db-up
docker compose up -d phoenix

printf 'phoenix-demo: waiting for %s\n' "$PHOENIX_URL"
attempts=0
while ! curl -fsS -o /dev/null "$PHOENIX_URL/v1/projects"; do
	attempts=$((attempts + 1))
	if [ "$attempts" -ge 60 ]; then
		printf 'phoenix-demo: Phoenix did not answer at %s\n' "$PHOENIX_URL" >&2
		docker compose logs --tail 40 phoenix >&2 || true
		exit 1
	fi
	sleep 1
done

printf 'phoenix-demo: running scripted trace + REST receipt check\n'
RECON_PHOENIX_E2E=1 \
	PHOENIX_URL="$PHOENIX_URL" \
	PHOENIX_OTLP_ENDPOINT="$PHOENIX_OTLP_ENDPOINT" \
	OTEL_SERVICE_NAME=recon-agent \
	OTEL_RESOURCE_ATTRIBUTES=deployment.environment=dev \
	go test -race -tags=integration -run '^TestPhoenixDemoReceipt$' -v -count=1 ./cmd/recon

printf '\nphoenix-demo: level 2 receipt PASS (see PHOENIX_LEVEL2 above)\n'
printf 'phoenix-demo: level 3 UI visibility PENDING (manual check): %s\n' "$PHOENIX_URL"
