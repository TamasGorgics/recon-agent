#!/bin/sh
# Negative lint check: prove that forbidigo rejects time.Now and time.Since in
# a domain package. The trap removes the injected file on every exit path.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"

TARGET="internal/ledger/zz_forbidigo_negative.go"
STATUS_BEFORE=$(git status --porcelain)

cleanup() {
	rm -f "$TARGET"
}
trap cleanup EXIT INT TERM

run_lint() {
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0 run ./internal/ledger
}

expect_rejected() {
	case_name=$1
	if output=$(run_lint 2>&1); then
		printf 'lint-negative: %s was not rejected\n%s\n' "$case_name" "$output" >&2
		exit 1
	fi
	if ! printf '%s\n' "$output" | grep -q 'forbidigo'; then
		printf 'lint-negative: %s failed for a reason other than forbidigo\n%s\n' "$case_name" "$output" >&2
		exit 1
	fi
	rm -f "$TARGET"
	printf 'lint-negative: %s rejected\n' "$case_name"
}

cat > "$TARGET" <<'EOF'
package ledger

import "time"

func ZZForbidigoNegativeNow() time.Time {
	return time.Now()
}
EOF
expect_rejected time.Now

cat > "$TARGET" <<'EOF'
package ledger

import "time"

func ZZForbidigoNegativeSince(t time.Time) time.Duration {
	return time.Since(t)
}
EOF
expect_rejected time.Since

if [ -e "$TARGET" ]; then
	printf 'lint-negative: %s still exists\n' "$TARGET" >&2
	exit 1
fi

STATUS_AFTER=$(git status --porcelain)
if [ "$STATUS_BEFORE" != "$STATUS_AFTER" ]; then
	printf 'lint-negative: working tree changed\nbefore:\n%s\nafter:\n%s\n' "$STATUS_BEFORE" "$STATUS_AFTER" >&2
	exit 1
fi

printf 'lint-negative: OK\n'
