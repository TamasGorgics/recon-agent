// Package dashboards owns the M5-04 operations-dashboard contract and its
// tests. There is no runtime service here: the database surface is the five
// owner-rights v_ops_* views and the recon_dashboards_ro read-only role, while
// the provisioned Grafana/Prometheus assets live under deploy/.
//
// The view contract is frozen by the M5-04 spec §3.4: match rate by UTC day and
// source, unmatched value by currency, open-break aging with the canonical
// 0-1 / 2-3 / 4-7 / >7 / unknown buckets, one row per proposal in the approval
// queue, and one escalated > human > auto classification per case.
package dashboards
