#!/bin/sh
# Verify the M5-04 Grafana/Prometheus provisioning against a running compose
# stack. Read-only: it never writes data or changes containers.
#
# Prerequisites:
#   docker compose up -d --wait postgres prometheus grafana
#   make migrate
# Existing Postgres volumes predate the dashboard login; rerun
# compose/initdb/01-roles.sql first (see its header) or recreate the volume.
#
# No secrets are stored here. Admin credentials come from the same env vars as
# compose, with the documented local-only defaults. Panel data may be empty
# until `make demo-ops` (W4); this script checks query execution, not coverage.
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

status, dashboard = request(f"{grafana}/api/dashboards/uid/recon-ops")
if status == 200 and dashboard.get("dashboard", {}).get("uid") == "recon-ops":
    meta = dashboard.get("meta", {})
    pass_(f"dashboard recon-ops provisioned={meta.get('provisioned')} folder={meta.get('folderTitle')}")
    panels = dashboard["dashboard"].get("panels", [])
else:
    fail(f"dashboard recon-ops: http={status}")
    panels = []


def query_panel(panel):
    target = panel["targets"][0]
    if panel["datasource"]["uid"] == "recon-postgres":
        body = {
            "queries": [{
                "refId": target.get("refId", "A"),
                "datasource": target["datasource"],
                "rawSql": target["rawSql"],
                "format": target.get("format", "table"),
                "editorMode": target.get("editorMode", "code"),
                "rawQuery": True,
                "intervalMs": 60000,
                "maxDataPoints": 100,
            }],
            "from": "now-14d",
            "to": "now",
        }
    else:
        body = {
            "queries": [{
                "refId": target.get("refId", "A"),
                "datasource": target["datasource"],
                "expr": target["expr"],
                "instant": True,
                "range": False,
            }],
            "from": "now-14d",
            "to": "now",
        }
    return request(f"{grafana}/api/ds/query", body)


for panel in sorted(panels, key=lambda item: item.get("id", 0)):
    status, body = query_panel(panel)
    result = body.get("results", {}).get("A", {})
    frames = result.get("frames", [])
    rows = 0
    for frame in frames:
        values = frame.get("data", {}).get("values", [])
        if values:
            rows = max(rows, len(values[0]))
    if status == 200 and result.get("status") == 200 and frames:
        pass_(f"panel {panel.get('id')} {panel.get('title')!r}: frames={len(frames)} rows={rows}")
        executed = json.dumps(result)
        if "$__" in executed:
            fail(f"panel {panel.get('id')} left an unexpanded macro")
    else:
        fail(f"panel {panel.get('id')} {panel.get('title')!r}: http={status} "
             f"query_status={result.get('status')} error={result.get('error')}")

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
print("\nverify-dashboard: OK (visual panel check remains PENDING/human)")
PY
