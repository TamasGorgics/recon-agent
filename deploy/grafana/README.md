# Grafana assets (M5-04)

Provisioned dev-only Grafana assets for the M5-04 ops dashboard:

- `provisioning/datasources/` — `recon-postgres` (read-only `v_ops_*` views)
  and `recon-prometheus` (self-scrape only).
- `provisioning/dashboards/provider.yaml` — file provider, folder `Recon`.
- `dashboards/ops.json` — the `Recon Ops` dashboard (`uid: recon-ops`).

Operator guide, bring-up/reset, panel catalog, security defaults, and the
still-PENDING human visual check: [`docs/dashboards.md`](../../docs/dashboards.md).

Quick start (from the repository root):

```sh
docker compose up -d --wait
make migrate
make demo-ops
./scripts/verify-dashboard.sh
```

Do not add literal credentials here; datasource credentials are `${...}`
placeholders filled from environment variables. The stack is dev-only:
Grafana and Prometheus are loopback-bound (the pre-existing Postgres publish is
not), and panel data comes from the synthetic `recon demo ops` seed — demo
evidence, not a production M2–M4 pipeline run. Prometheus is a self-scrape, not
live application metrics.
