# M2 fixture

`eval/fixtures/m2` is the deterministic three-case fixture used by the M2
cassette recordings, the CLI smoke test, and the `TestMilestone2Gate` exit gate.
It is eval-only: the production `recon demo seed` path is unchanged.

## Provenance

- The fixture is a Go loader (`fixture.go` + `data.go`) with fixed SQL
  constants, not a generated dataset. The production `synth.Load` assigns
  random account, scope, and case ids, which would invalidate recorded cassette
  keys.
- Every timestamp derives from `FixedInstant` = `2026-01-15T00:00:00Z`; every
  id is a fixed UUID constant. Changing either changes the tool transcripts and
  requires re-recording the cassettes.
- All memos and record references are synthetic (`Synthetic monthly processing
  charge`, `Synthetic subscription charge`, `Synthetic duplicate candidate`) and
  no real account, counterparty, or payment data is present.
- The ledger entries are balanced per transaction, and the adjustment accounts
  in the fixture are allowlisted for the proposal tools.

## Fixed scenarios

| Case id | Scenario | Expected proposal |
|---|---|---|
| `10000000-0000-4000-8000-000000000001` | `fee_variance` | `adjustment` |
| `10000000-0000-4000-8000-000000000002` | `reference_typo` | `link` |
| `10000000-0000-4000-8000-000000000003` | `duplicate_charge` | `duplicate` |

The gate adds a fourth case id (`...0004`) at runtime for the simulated
provider-outage path; it is not part of `Scenarios()`.

## Fresh-clone requirement

`Load` refuses to run twice: ledger rows are immutable, so it checks for the
fixed case ids and returns `ErrAlreadyLoaded` instead of overwriting. Callers
must use a fresh database clone (`testdb.New`) or reset the database. Scripted
replay also uses deterministic run ids; replaying onto a database that already
holds those runs fails loudly on the primary key, and the CLI error tells the
operator to clone/reset the fixture.

## Commands

```bash
# Fixture determinism (two fresh clones produce identical projections)
TEST_DATABASE_URL=postgres://recon:recon@localhost:5432/recon?sslmode=disable \
  go test -race -tags=integration -run TestM2Fixture ./eval/fixtures/m2

# M2 exit gate (replays committed cassettes, no API key)
make demo-m2
```

Recording is manual only and never runs in CI. See
`eval/cassettes/README.md` for the re-record procedure and the freeze rule.
