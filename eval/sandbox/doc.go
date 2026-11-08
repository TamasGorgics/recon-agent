// Package sandbox clones an isolated Postgres database for one eval world and
// makes its cases available to the eval runner.
//
// # Shared-world model
//
// A sandbox is one database cloned from a migrated template. One generated
// world (many scenarios) is loaded into that clone once. Each scoreable
// scenario gets exactly one case, created by a one-candidate
// cases.CreateBreaks call so production batch grouping cannot merge two
// scenarios into a single case. Parallel sandboxes are supported: every clone
// and every ephemeral login has a process-unique name and the template setup
// is serialized by internal/testdb's advisory lock.
//
// # Ground-truth isolation
//
// Ground truth lives in the eval_truth schema created by the
// 20261108120000_m3_02_eval_truth migration. The schema is present in the
// template and cloned into every sandbox. PUBLIC is revoked and none of
// recon_app, recon_agent_ro, recon_audit_ro, or recon_executor receives any
// privilege on it. The loader and the trusted eval runner use the admin pool
// only; the agent tool pools are opened through db.OpenRole from ephemeral
// role logins and cannot read eval_truth.
//
// # Synthetic eval-only cases
//
// Most generated failure scenarios have a non-empty break signature. An
// injection_memo scenario intentionally has no production break: its memo text
// is evidence only and the expected outcome is escalate. The sandbox creates an
// eval-only case for it from an external-only break derived from the truth
// references, with eval_truth.scenario_cases.synthetic = true and break_id
// NULL. That path exists only in eval/sandbox and must never be copied into a
// production code path.
//
// # Truth candidate reconstruction
//
// eval/dataset ground-truth break signatures are computed from the dataset's
// deterministic logical scope ids and the generator's canonical break shape.
// A cloned database assigns random reconciliation_scopes UUIDs and the
// production matcher's break candidates use different shapes (for example an
// external-only break with a zero delta), so those signatures do not equal the
// matcher's signatures for most modes. To keep eval_truth.scenario_cases
// keyed by the committed truth signature, Clone pins each clone's scope ids to
// the same deterministic logical ids the dataset used and reconstructs the
// unique truth candidate from the scenario's own ledger and external
// references. The reconstructed candidate must hash to truth.BreakSignature
// exactly; zero or multiple reconstructions fail closed. The production
// matcher still runs for every scope so matched rows and match_runs exist.
//
// # Naming and cleanup
//
// Template: recon_eval_template (overridable through Options.Template).
// Clone: recon_eval_<pid>_<counter>_<random>.
// Ephemeral logins: recon_eval_login_<pid>_<counter>_<random>_<role-suffix>.
// The recon_eval_ prefix never matches internal/testdb's recon_test_% cleanup,
// and CleanupOrphans/CleanupOrphanLogins only touch recon_eval_% names.
package sandbox
