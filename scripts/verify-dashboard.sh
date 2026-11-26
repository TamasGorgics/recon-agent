#!/bin/sh
# Verify the Grafana/Prometheus provisioning for both M5-04 (`recon-ops`) and
# M5-05 (`recon-agent-quality`). Read-only: it never writes data or changes
# containers.
#
# Prerequisites:
#   docker compose up -d --wait postgres prometheus grafana
#   make migrate
#   make demo-quality          # M1 + ops + quality synthetic rows
# Existing Postgres volumes predate the dashboard login; rerun
# compose/initdb/01-roles.sql first (see its header) or recreate the volume.
#
# No secrets are stored here. Admin credentials come from the same env vars as
# compose, with the documented local-only defaults. After `make demo-quality`
# every panel must return rows; an empty panel is a failure, not a skip.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"

GRAFANA_URL=${RECON_GRAFANA_URL:-http://127.0.0.1:${GRAFANA_PORT:-3000}}
PROMETHEUS_URL=${PROMETHEUS_URL:-http://127.0.0.1:${PROMETHEUS_PORT:-9090}}
RECON_GRAFANA_ADMIN_USER=${RECON_GRAFANA_ADMIN_USER:-admin}
RECON_GRAFANA_ADMIN_PASSWORD=${RECON_GRAFANA_ADMIN_PASSWORD:-recon_admin_local_only}
export GRAFANA_URL PROMETHEUS_URL RECON_GRAFANA_ADMIN_USER RECON_GRAFANA_ADMIN_PASSWORD

docker compose config -q
printf 'PASS compose config\n'

python3 - <<'PY'
import base64
import json
import os
import sys
import urllib.error
import urllib.request

grafana = os.environ["GRAFANA_URL"].rstrip("/")
prometheus = os.environ["PROMETHEUS_URL"].rstrip("/")
user = os.environ["RECON_GRAFANA_ADMIN_USER"]
password = os.environ["RECON_GRAFANA_ADMIN_PASSWORD"]
auth = "Basic " + base64.b64encode(f"{user}:{password}".encode()).decode()

failures = []


def pass_(message):
    print(f"PASS {message}")


def fail(message):
    failures.append(message)
    print(f"FAIL {message}")


def request(url, body=None, auth_required=True):
    headers = {"Content-Type": "application/json"}
    if auth_required:
        headers["Authorization"] = auth
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(url, data=data, headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=30) as response:
            return response.status, decode(response.read())
    except urllib.error.HTTPError as err:
        return err.code, decode(err.read())
    except urllib.error.URLError as err:
        fail(f"cannot reach {url}: {err.reason}")
        return 0, {}


def decode(raw):
    try:
        return json.loads(raw.decode())
    except Exception:
        return raw.decode(errors="replace")


status, health = request(f"{grafana}/api/health")
if status == 200 and health.get("database") == "ok":
    pass_(f"Grafana /api/health ({health.get('version')})")
else:
    fail(f"Grafana /api/health: http={status} body={health}")

status, ready = request(f"{prometheus}/-/ready", auth_required=False)
if status == 200:
    pass_("Prometheus /-/ready")
else:
    fail(f"Prometheus /-/ready: http={status}")

# Anonymous dashboard reads must be rejected (Grafana auth is enabled).
status, _ = request(f"{grafana}/api/dashboards/uid/recon-agent-quality", auth_required=False)
if status == 401:
    pass_("anonymous dashboard API denied with HTTP 401")
else:
    fail(f"anonymous dashboard API returned HTTP {status}, want 401")

status, datasources = request(f"{grafana}/api/datasources")
by_uid = {ds.get("uid"): ds for ds in datasources} if isinstance(datasources, list) else {}
for uid in ("recon-postgres", "recon-prometheus"):
    if uid in by_uid:
        pass_(f"datasource provisioned: {uid}")
    else:
        fail(f"datasource missing: {uid}")

for uid in ("recon-postgres", "recon-prometheus"):
    if uid not in by_uid:
        continue
    status, health = request(f"{grafana}/api/datasources/uid/{uid}/health")
    if status == 200 and health.get("status") == "OK":
        pass_(f"datasource health {uid}: {health.get('message')}")
    else:
        fail(f"datasource health {uid}: http={status} body={health}")


def query_panel(panel):
    targets = panel.get("targets", [])
    datasource_uid = panel.get("datasource", {}).get("uid")
    if datasource_uid == "recon-postgres":
        queries = [{
            "refId": target.get("refId", "A"),
            "datasource": target.get("datasource", panel["datasource"]),
            "rawSql": target["rawSql"],
            "format": target.get("format", "table"),
            "editorMode": target.get("editorMode", "code"),
            "rawQuery": True,
            "intervalMs": 60000,
            "maxDataPoints": 100,
        } for target in targets]
    else:
        queries = [{
            "refId": target.get("refId", "A"),
            "datasource": target.get("datasource", panel["datasource"]),
            "expr": target["expr"],
            "instant": True,
            "range": False,
        } for target in targets]
    body = {"queries": queries, "from": "now-14d", "to": "now"}
    return request(f"{grafana}/api/ds/query", body)


# UID, title, panel count, and whether the panel inventory includes the
# escalation-reasons table (which must have at least three reasons).
dashboard_contracts = [
    ("recon-ops", "Recon Ops", 6, False),
    ("recon-agent-quality", "Recon Agent Quality", 8, True),
]

for uid, title, expected_panels, reasons_table in dashboard_contracts:
    status, dashboard = request(f"{grafana}/api/dashboards/uid/{uid}")
    if status != 200 or dashboard.get("dashboard", {}).get("uid") != uid:
        fail(f"dashboard {uid}: http={status}")
        continue
    meta = dashboard.get("meta", {})
    dash = dashboard["dashboard"]
    if dash.get("title") != title:
        fail(f"dashboard {uid} title = {dash.get('title')!r}, want {title!r}")
    if meta.get("provisioned") is not True:
        fail(f"dashboard {uid} provisioned = {meta.get('provisioned')!r}, want true")
    else:
        pass_(f"dashboard {uid} provisioned={meta.get('provisioned')} folder={meta.get('folderTitle')}")
    if meta.get("folderTitle") != "Recon":
        fail(f"dashboard {uid} folder = {meta.get('folderTitle')!r}, want 'Recon'")
    if int(dash.get("version", 0)) < 1:
        fail(f"dashboard {uid} version = {dash.get('version')!r}, want >= 1")
    panels = sorted(dash.get("panels", []), key=lambda item: item.get("id", 0))
    if len(panels) != expected_panels:
        fail(f"dashboard {uid} panels = {len(panels)}, want {expected_panels}")

    reasons_rows = 0
    for panel in panels:
        status, body = query_panel(panel)
        results = body.get("results", {}) if isinstance(body, dict) else {}
        panel_ok = status == 200
        rows = 0
        for target in panel.get("targets", []):
            ref = target.get("refId", "A")
            result = results.get(ref, {})
            frames = result.get("frames", [])
            target_rows = 0
            for frame in frames:
                values = frame.get("data", {}).get("values", [])
                if values:
                    target_rows = max(target_rows, len(values[0]))
            if status != 200 or result.get("status") != 200 or not frames or target_rows == 0:
                panel_ok = False
                fail(f"dashboard {uid} panel {panel.get('id')} {panel.get('title')!r} "
                     f"target {ref}: http={status} query_status={result.get('status')} "
                     f"frames={len(frames)} rows={target_rows} error={result.get('error')}")
            else:
                executed = json.dumps(result)
                if "$__" in executed:
                    fail(f"dashboard {uid} panel {panel.get('id')} left an unexpanded macro")
                rows = max(rows, target_rows)
        if panel_ok:
            pass_(f"dashboard {uid} panel {panel.get('id')} {panel.get('title')!r}: rows={rows}")
        if panel.get("id") == 7:
            reasons_rows = rows

    if reasons_table and reasons_rows < 3:
        fail(f"dashboard {uid} escalation-reasons table rows = {reasons_rows}, want >= 3")

status, body = query_panel({
    "id": "denial",
    "title": "base-table denial",
    "datasource": {"uid": "recon-postgres"},
    "targets": [{
        "refId": "A",
        "datasource": {"type": "grafana-postgresql-datasource", "uid": "recon-postgres"},
        "rawSql": "SELECT count(*) FROM matches",
        "format": "table",
    }],
})
error = json.dumps(body)
if status == 400 and "42501" in error:
    pass_("dashboard role denied on base table matches (SQLSTATE 42501)")
else:
    fail(f"base table matches was not denied: http={status} body={error[:300]}")

if failures:
    print(f"\nverify-dashboard: {len(failures)} failure(s)")
    sys.exit(1)
print("\nverify-dashboard: OK (both dashboards; visual panel check remains PENDING/human)")
PY
