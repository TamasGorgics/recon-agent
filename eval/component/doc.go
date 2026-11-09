// Package component produces the standalone matcher component report (M3-06).
//
// The matcher report scores the production matcher against the committed eval
// dataset truth using internal/matchscore, exactly as the M1-16 component gate
// does. It is standalone: it creates a disposable eval/sandbox clone, reads the
// active matches through the recon_app role, renders a matcher-report/v1
// document, and writes no eval_runs or eval_case_results rows.
//
// The dataset split is loaded, rebuilt and verified fail-closed against the
// committed manifest. Recall below 1.0 is a finding, not a command failure;
// known ADR 0004 residuals are expected and are not special-cased here.
//
// Matcher reports contain no transcripts, money, prompts, or raw tool data.
// They are synthetic-data tooling; the report does not claim PII/PAN scrubbing.
package component
