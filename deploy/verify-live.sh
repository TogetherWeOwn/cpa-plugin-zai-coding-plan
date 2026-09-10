#!/usr/bin/env bash
set -euo pipefail

: "${CLIPROXY_MANAGEMENT_URL:=http://127.0.0.1:8317}"
: "${CLIPROXY_USAGE_DIR:=/srv/cliproxy-usage}"
: "${CLIPROXY_MANAGEMENT_KEY_FILE:?set CLIPROXY_MANAGEMENT_KEY_FILE to a root-readable 0600 file}"
: "${ZAI_CODING_PLAN_KEY_FILE:?set ZAI_CODING_PLAN_KEY_FILE to a root-readable 0600 file}"
: "${CLIPROXY_DASHBOARD_URL:=http://127.0.0.1:3000/api/telemetry/model-usage/zai}"
: "${CLIPROXY_SERVICE_UNIT:=cliproxy.service}"
: "${CLIPROXY_JOURNAL_TIMEOUT:=5}"

umask 077
work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT
status_file="$work_dir/status.json"
dashboard_file="$work_dir/dashboard.json"
log_file="$work_dir/service.log"
curl_config="$work_dir/management.curl"
status_url="${CLIPROXY_MANAGEMENT_URL%/}/v0/management/plugins/zai-coding-plan/status"

python3 - "$CLIPROXY_MANAGEMENT_URL" "$status_url" <<'PY'
import sys, urllib.parse
allowed={"http://127.0.0.1:8317", "http://[::1]:8317", "http://localhost:8317"}
def origin(value):
    parsed=urllib.parse.urlsplit(value)
    if parsed.scheme not in {"http","https"} or not parsed.hostname or parsed.username or parsed.password:
        raise SystemExit("management URL must be an absolute HTTP(S) URL without userinfo")
    if parsed.query or parsed.fragment:
        raise SystemExit("management URL must not contain a query or fragment")
    try:
        port=parsed.port
    except ValueError:
        raise SystemExit("management URL has an invalid port")
    host=parsed.hostname.lower()
    rendered=f"[{host}]" if ":" in host else host
    return f"{parsed.scheme}://{rendered}:{port or (80 if parsed.scheme == 'http' else 443)}"
base, status=sys.argv[1:]
if origin(base) not in {origin(value) for value in allowed}:
    raise SystemExit("management URL origin is not approved")
parsed=urllib.parse.urlsplit(status)
if parsed.path != "/v0/management/plugins/zai-coding-plan/status" or origin(status) != origin(base):
    raise SystemExit("management status URL is not approved")
PY

python3 - "$CLIPROXY_MANAGEMENT_KEY_FILE" "$curl_config" <<'PY'
import pathlib, sys
raw=pathlib.Path(sys.argv[1]).read_bytes()
lines=raw.splitlines()
if len(lines) != 1 or not lines[0] or lines[0].strip() != lines[0]:
    raise SystemExit("management key file must contain exactly one non-empty line")
try:
    key=lines[0].decode("utf-8")
except UnicodeDecodeError:
    raise SystemExit("management key file must contain valid UTF-8")
path=pathlib.Path(sys.argv[2])
path.write_text('header = "Authorization: Bearer ' + key.replace('\\', '\\\\').replace('"', '\\"') + '"\n')
path.chmod(0o600)
PY

unauthenticated=$(curl --fail-early --max-redirs 0 --silent --show-error --max-time 5 --max-filesize 1048576 --output /dev/null --write-out '%{http_code}' "$status_url")
test "$unauthenticated" = 401

curl --fail-with-body --fail-early --max-redirs 0 --silent --show-error \
  --max-time 5 --max-filesize 1048576 \
  --config "$curl_config" \
  "$status_url" >"$status_file"

python3 - "$status_file" "$CLIPROXY_MANAGEMENT_KEY_FILE" "$ZAI_CODING_PLAN_KEY_FILE" <<'PY'
import json, math, pathlib, re, sys
expected_top={"plugin","status","version","generated_at","accounts"}
required={"name","key_suffix","plan","five_hour_utilization","weekly_utilization","five_hour_resets_at","weekly_resets_at","quota_source","quota_observed_at","quota_age_seconds","quota_stale","offpeak","health","estimator_complete_since","delivery_warning","persistence_warning","unknown_model_warning","heuristic_dedup_warning","dedup_mode"}
rfc3339=re.compile(r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$")
def read_secret(path):
    raw=pathlib.Path(path).read_bytes()
    lines=raw.splitlines()
    if len(lines) != 1 or not lines[0] or lines[0].strip() != lines[0]:
        raise SystemExit("secret input file must contain exactly one non-empty line")
    return lines[0]
raw=pathlib.Path(sys.argv[1]).read_bytes()
if len(raw) > 1_048_576:
    raise SystemExit("authenticated status response exceeds bounded scan size")
management=read_secret(sys.argv[2])
plan=read_secret(sys.argv[3])
markers=[management, plan]
if len(plan) > 6:
    markers.append(plan[-6:])
if any(marker in raw for marker in markers):
    raise SystemExit("authenticated status response failed confidential-value scan")
def reject_constant(_):
    raise ValueError("non-RFC JSON value")
status=json.loads(raw, parse_constant=reject_constant)
assert set(status) == expected_top
assert status["plugin"]=="zai-coding-plan" and status["status"]=="registered"
assert isinstance(status["version"], str) and status["version"]
assert isinstance(status["generated_at"], str) and rfc3339.fullmatch(status["generated_at"])
assert status["accounts"]
for account in status["accounts"]:
    assert required <= set(account) and set(account) <= required | {"quota_error"}
    assert isinstance(account["name"], str) and account["name"]
    assert account["key_suffix"] == "redacted"
    assert account["quota_source"] in {"quota_api","estimate"}
    assert isinstance(account["quota_age_seconds"], int) and not isinstance(account["quota_age_seconds"], bool) and account["quota_age_seconds"] >= 0
    assert all(account[field] is None or isinstance(account[field], str) and rfc3339.fullmatch(account[field]) for field in ("five_hour_resets_at","weekly_resets_at","estimator_complete_since"))
    assert isinstance(account["quota_observed_at"], str) and rfc3339.fullmatch(account["quota_observed_at"])
    assert all(isinstance(account[field], (int,float)) and not isinstance(account[field], bool) and math.isfinite(account[field]) and 0 <= account[field] <= 1 for field in ("five_hour_utilization","weekly_utilization"))
    assert not any(re.search(r"(?:api[-_]?key|authorization|credential|secret|token|key_hash|identity)", key, re.I) for key in account)
PY

"${COLLECTOR_ZAI:-$(dirname "$0")/collector-zai.py}" \
  --management-key-file "$CLIPROXY_MANAGEMENT_KEY_FILE" \
  --secret-marker-file "$CLIPROXY_MANAGEMENT_KEY_FILE" \
  --secret-marker-file "$ZAI_CODING_PLAN_KEY_FILE" \
  --url "$status_url"

curl --fail-with-body --fail-early --max-redirs 0 --silent --show-error \
  --max-time 5 --max-filesize 1048576 \
  "$CLIPROXY_DASHBOARD_URL" >"$dashboard_file"

timeout --foreground --signal=TERM --kill-after=1 "$CLIPROXY_JOURNAL_TIMEOUT" \
  journalctl --unit "$CLIPROXY_SERVICE_UNIT" --since '-15 minutes' --no-pager --output=cat --lines=2000 \
  | python3 -c 'import sys; raw=sys.stdin.buffer.read(1_048_577); sys.stdout.buffer.write(raw); raise SystemExit(len(raw) > 1_048_576)' \
  >"$log_file"

python3 - "$CLIPROXY_USAGE_DIR/zai.json" "$dashboard_file" "$log_file" "$CLIPROXY_MANAGEMENT_KEY_FILE" "$ZAI_CODING_PLAN_KEY_FILE" <<'PY'
import json, pathlib, re, sys
projected, dashboard, service_log = map(pathlib.Path, sys.argv[1:4])
def read_secret(path):
    raw=pathlib.Path(path).read_bytes()
    lines=raw.splitlines()
    if len(lines) != 1 or not lines[0] or lines[0].strip() != lines[0]:
        raise SystemExit("secret input file must contain exactly one non-empty line")
    return lines[0]
management=read_secret(sys.argv[4])
plan=read_secret(sys.argv[5])
markers=[]
markers=[management, plan]
if len(plan) > 6:
    markers.append(plan[-6:])
for path in (projected, dashboard, service_log):
    raw = path.read_bytes()
    if len(raw) > 1_048_576:
        raise SystemExit(f"{path.name} exceeds bounded scan size")
    if any(marker in raw for marker in markers):
        raise SystemExit(f"{path.name} failed confidential-value scan")
def reject_constant(_):
    raise ValueError("non-RFC JSON value")
payload=json.loads(projected.read_text(), parse_constant=reject_constant)
assert payload["schemaVersion"]==1 and payload["lane"]=="zai" and payload["records"]
serialized=json.dumps(payload, allow_nan=False)
assert not re.search(r'"(?:api[-_]?key|authorization|credential|secret|token|key_hash|identity)"\s*:', serialized, re.I)
assert all(record["key_suffix"]=="redacted" for record in payload["records"])
PY

printf 'dogfood-live: authenticated status, collector lane, dashboard, service-log, and bounded secret-marker scans PASS\n'
