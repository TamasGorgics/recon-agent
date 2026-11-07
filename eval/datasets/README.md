# Eval datasets

`eval/datasets` contains the committed provenance manifests for the three
deterministic M3 eval splits. The generated worlds themselves are **never
committed**: `ledger.json`, `psp_a/*.csv`, `bank_x/*.csv`, and
`ground_truth.json` are rebuilt into a temporary directory from the pinned
generator/seed/config.

## Committed content

- `dev/manifest.json`, `test/manifest.json`, `injection/manifest.json` — one
  manifest per split (about 31 KiB each, total below the 256 KiB budget).
- This README.

Everything else under a split directory is a regeneration artifact and must
stay out of Git. `TestCommittedManifestHygieneAndBudget` fails if any other file
appears here.

## Locked splits

| Split | Seed | Days | Difficulty | Applications |
|---|---:|---:|---|---|
| `dev` | 1 | 35 | `realistic` | 12 per mode; `batch_composite` 6 |
| `test` | 2 | 35 | `realistic` | 30 per mode; `batch_composite` 15 |
| `injection` | 3 | 16 | `adversarial` | 30 `injection_memo` only |

`batch_composite` emits two truth rows per application (composite link plus
missing charge), so `test` has 30 `batch_composite` truth rows from 15
applications. The injection split selects only `injection_memo` truth; the
clean generator rows stay in the world files but are not scored.

## Manifest and versioning

The manifest records the generator version, generator dataset version, split
seed/days/difficulty, canonical synth and modes configuration, noise knobs,
source-calendar canonical string, SHA-256 of every resolved injection template,
truth schema version, world counts, per-mode/application/difficulty counts, and
the SHA-256 of every generated world file.

`eval_dataset_version` is SHA-256 over the canonical compact JSON of every
manifest field except itself. It is the value recorded on
`eval_runs.dataset_version`. `ground_truth.json` deliberately does not carry
`eval_dataset_version`; it carries `generator_dataset_version` only, so the hash
is not circular. The eval version covers template and source-calendar content
that `synth`'s own `dataset_version` does not include (Q-40).

The eval `LedgerConfig.AdjustmentAccounts` allowlist uses concrete
generator-suffixed accounts: `clearing_psp_a_<ccy>`, `fees_expense_<ccy>`, and
`refunds_expense_<ccy>` for every dataset currency. `fx_rounding` uses
`fees_expense_<ccy>` as the designated credit account because the generator has
no rounding/FX account (Q-41).

## Regeneration

```bash
# Rebuild all three splits, compare their manifests, and update the committed
# files byte-for-byte. Run only when the generator/config change is intentional.
go test ./eval/dataset -run TestCommittedManifests -update

# Verify the committed manifests match a fresh deterministic build.
go test ./eval/dataset

# Full repository checks (dataset tests need no database).
make lint test
make test-integration   # includes the eval migration/grant integration test
```

No database is required to generate or validate a split. `Build` never reads the
wall clock or the network; the fake clock is provenance only.
