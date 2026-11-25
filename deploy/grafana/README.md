# Grafana assets (M5-04, extended by M5-05)

Provisioned dev-only Grafana assets for the M5-04 ops dashboard and the M5-05
agent-quality dashboard, both on the same read-only datasource surface:

- `provisioning/datasources/` — `recon-postgres` (read-only `v_ops_*` and
  `v_eval_*` views) and `recon-prometheus` (self-scrape only).
- `provisioning/dashboards/provider.yaml` — file provider, folder `Recon`.
- `dashboards/ops.json` — the `Recon Ops` dashboard (`uid: recon-ops`, six
  panels).
- `dashboards/agent-quality.json` — the `Recon Agent Quality` dashboard
  (`uid: recon-agent-quality`, eight panels).

Operator guide for the shared stack, bring-up/reset, view/role surface,
security defaults, and the still-PENDING human visual check:
[`docs/dashboards.md`](../../docs/dashboards.md). The agent-quality panel
catalog, seed/reset boundary, and visual checklist are in
[`docs/agent-quality.md`](../../docs/agent-quality.md).

Quick start (from the repository root):

```sh
docker compose up -d --wait
make migrate
make demo-quality      # M1 + ops + quality synthetic rows; populates both dashboards
./scripts/verify-dashboard.sh
```

`make demo-ops` populates only the ops dashboard and leaves the quality panels
empty. If a long-running Grafana container does not see the new
`agent-quality.json`, restart Grafana once to refresh the bind mount.

Do not add literal credentials here; datasource credentials are `${...}`
placeholders filled from environment variables. The stack is dev-only:
Grafana and Prometheus are loopback-bound (the pre-existing Postgres publish is
not), and panel data comes from the synthetic `recon demo quality` seed — demo
evidence, not a production M2–M4 pipeline run or eval result. Prometheus is a
self-scrape, not live application metrics. Q-103 records the quality-dashboard
boundaries and the still-open spec/docs conflicts; `docs/spec.md` is unchanged.
