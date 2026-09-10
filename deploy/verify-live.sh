#!/usr/bin/env bash
set -euo pipefail

: "${CLIPROXY_MANAGEMENT_URL:=http://127.0.0.1:8317}"
: "${CLIPROXY_USAGE_DIR:=/srv/cliproxy-usage}"
: "${CLIPROXY_MANAGEMENT_KEY_FILE:?set CLIPROXY_MANAGEMENT_KEY_FILE to a root-readable 0600 file}"
: "${ZAI_CODING_PLAN_KEY_FILE:?set ZAI_CODING_PLAN_KEY_FILE to a root-readable 0600 file}"
: "${CLIPROXY_DASHBOARD_URL:=http://127.0.0.1:3000/api/telemetry/model-usage/zai}"
: "${CLIPROXY_SERVICE_UNIT:=cliproxy.service}"

umask 077
work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT
status_file="$work_dir/status.json"
dashboard_file="$work_dir/dashboard.json"
log_file="$work_dir/service.log"
curl_config="$work_dir/management.curl"
status_url="${CLIPROXY_MANAGEMENT_URL%/}/v0/management/plugins/zai-coding-plan/status"

python3 - "$CLIPROXY_MANAGEMENT_KEY_FILE" "$curl_config" <<'PY'
import pathlib, sys
key=pathlib.Path(sys.argv[1]).read_text().strip()
if not key or "\n" in key or "\r" in key:
    raise SystemExit("management key file must contain one non-empty line")
path=pathlib.Path(sys.argv[2])
path.write_text('header = "Authorization: Bearer ' + key.replace('\\', '\\\\').replace('"', '\\"') + '"\n')
path.chmod(0o600)
PY

unauthenticated=$(curl --fail-early --max-redirs 0 --silent --show-error --output /dev/null --write-out '%{http_code}' "$status_url")
test "$unauthenticated" = 401

curl --fail-with-body --fail-early --max-redirs 0 --silent --show-error \
  --config "$curl_config" \
  "$status_url" >"$status_file"

python3 - "$status_file" <<'PY'
import json, re, sys
status=json.load(open(sys.argv[1]))
required={"name","key_suffix","plan","five_hour_utilization","weekly_utilization","five_hour_resets_at","weekly_resets_at","quota_source","quota_observed_at","quota_age_seconds","quota_stale","offpeak","health","estimator_complete_since","delivery_warning","persistence_warning","unknown_model_warning","heuristic_dedup_warning","dedup_mode"}
assert status["plugin"]=="zai-coding-plan" and status["status"]=="registered"
assert status["accounts"]
for account in status["accounts"]:
    assert required <= set(account)
    assert account["key_suffix"] == "redacted"
    assert account["quota_source"] in {"quota_api","estimate"}
    assert 0 <= account["five_hour_utilization"] <= 1
    assert 0 <= account["weekly_utilization"] <= 1
    assert not any(re.search(r"(?:api[-_]?key|authorization|credential|secret|token|key_hash|identity)", key, re.I) for key in account)
PY

"${COLLECTOR_ZAI:-$(dirname "$0")/collector-zai.py}" \
  --management-key-file "$CLIPROXY_MANAGEMENT_KEY_FILE" \
  --secret-marker-file "$CLIPROXY_MANAGEMENT_KEY_FILE" \
  --secret-marker-file "$ZAI_CODING_PLAN_KEY_FILE" \
  --url "$status_url" \
  --output "$CLIPROXY_USAGE_DIR/zai.json"

curl --fail-with-body --fail-early --max-redirs 0 --silent --show-error \
  --max-time 5 --max-filesize 1048577 \
  "$CLIPROXY_DASHBOARD_URL" >"$dashboard_file"

journalctl --unit "$CLIPROXY_SERVICE_UNIT" --since '-15 minutes' --no-pager --output=cat --lines=2000 >"$log_file"

python3 - "$CLIPROXY_USAGE_DIR/zai.json" "$dashboard_file" "$log_file" "$CLIPROXY_MANAGEMENT_KEY_FILE" "$ZAI_CODING_PLAN_KEY_FILE" <<'PY'
import json, pathlib, re, sys
projected, dashboard, service_log = map(pathlib.Path, sys.argv[1:4])
management = pathlib.Path(sys.argv[4]).read_text().strip().encode()
plan = pathlib.Path(sys.argv[5]).read_text().strip().encode()
markers = tuple(value for value in (management, plan) if value)
for path in (projected, dashboard, service_log):
    raw = path.read_bytes()
    if len(raw) > 1_048_576:
        raise SystemExit(f"{path.name} exceeds bounded scan size")
    if any(marker in raw for marker in markers):
        raise SystemExit(f"{path.name} contains a secret marker")
payload=json.loads(projected.read_text())
assert payload["schemaVersion"]==1 and payload["lane"]=="zai" and payload["records"]
serialized=json.dumps(payload)
assert not re.search(r'"(?:api[-_]?key|authorization|credential|secret|token|key_hash|identity)"\s*:', serialized, re.I)
assert all(record["key_suffix"]=="redacted" for record in payload["records"])
PY

printf 'dogfood-live: authenticated status, collector lane, dashboard, service-log, and bounded secret-marker scans PASS\n'
