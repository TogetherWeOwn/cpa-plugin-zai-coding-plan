#!/usr/bin/env bash
set -euo pipefail

: "${CLIPROXY_MANAGEMENT_URL:=http://127.0.0.1:8317}"
: "${CLIPROXY_USAGE_DIR:=/srv/cliproxy-usage}"
: "${CLIPROXY_MANAGEMENT_KEY_FILE:?set CLIPROXY_MANAGEMENT_KEY_FILE to a root-readable 0600 file}"
: "${ZAI_CODING_PLAN_KEY_FILE:?set ZAI_CODING_PLAN_KEY_FILE to a root-readable 0600 file}"
: "${CLIPROXY_DASHBOARD_URL:=http://127.0.0.1:3000/api/telemetry/model-usage/zai}"
: "${CLIPROXY_SERVICE_UNIT:=cliproxy.service}"
: "${CLIPROXY_JOURNAL_TIMEOUT:=5}"
: "${CLIPROXY_EXPECTED_PLUGIN_VERSION:?set CLIPROXY_EXPECTED_PLUGIN_VERSION to the installed release}"

python3 - "$CLIPROXY_JOURNAL_TIMEOUT" <<'PY'
import re, sys
value=sys.argv[1]
if len(value) > 16 or not re.fullmatch(r"(?:[1-9]\d*(?:\.\d+)?|0\.\d*[1-9]\d*)[smh]?", value):
    raise SystemExit("CLIPROXY_JOURNAL_TIMEOUT must be a positive duration using s, m, or h")
PY

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

# Every secret file is opened with O_NOFOLLOW and must be a regular file owned by the
# effective user, with no group/other access, and no larger than the bound below. A
# symlink, FIFO, device, foreign owner, or loose mode is rejected before any read.
python3 - "$CLIPROXY_MANAGEMENT_KEY_FILE" "$ZAI_CODING_PLAN_KEY_FILE" <<'PY'
import os, stat, sys
MAX_SECRET_BYTES = 65536
for path in sys.argv[1:]:
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_CLOEXEC | os.O_NONBLOCK)
    except OSError as error:
        raise SystemExit(f"secret input file could not be opened without following a symlink: {error.strerror}")
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode):
            raise SystemExit("secret input file must be a regular file")
        if info.st_uid != os.geteuid():
            raise SystemExit("secret input file must be owned by the effective user")
        if info.st_mode & 0o077:
            raise SystemExit("secret input file must not be group- or world-accessible")
        if info.st_size > MAX_SECRET_BYTES:
            raise SystemExit("secret input file exceeds the bounded size")
    finally:
        os.close(fd)
PY

python3 - "$CLIPROXY_MANAGEMENT_KEY_FILE" "$curl_config" <<'PY'
import os, pathlib, stat, sys
MAX_SECRET_BYTES = 65536
fd=os.open(sys.argv[1], os.O_RDONLY | os.O_NOFOLLOW | os.O_CLOEXEC | os.O_NONBLOCK)
try:
    info=os.fstat(fd)
    if not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o077:
        raise SystemExit("management key file failed type, owner, or mode enforcement")
    if info.st_size > MAX_SECRET_BYTES:
        raise SystemExit("management key file exceeds the bounded size")
    with os.fdopen(os.dup(fd), "rb") as handle:
        raw=handle.read(MAX_SECRET_BYTES + 1)
finally:
    os.close(fd)
if len(raw) > MAX_SECRET_BYTES:
    raise SystemExit("management key file exceeds the bounded size")
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

unauthenticated=$(curl -q --fail-early --max-redirs 0 --silent --show-error --connect-timeout 2 --max-time 5 --max-filesize 1048576 --noproxy '*' --proxy '' --output /dev/null --write-out '%{http_code}' "$status_url")
test "$unauthenticated" = 401

curl -q --fail-with-body --fail-early --max-redirs 0 --silent --show-error \
  --connect-timeout 2 --max-time 5 --max-filesize 1048576 --noproxy '*' --proxy '' \
  --config "$curl_config" \
  "$status_url" >"$status_file"

set +e
python3 - "$status_file" <<'PY'
import json, pathlib, sys
try:
    status=json.loads(pathlib.Path(sys.argv[1]).read_bytes())
except (OSError, UnicodeError, json.JSONDecodeError):
    raise SystemExit(1)
if isinstance(status, dict) and status.get("status") == "reconfigure_rejected" and isinstance(status.get("validation_error"), str):
    raise SystemExit(10)
raise SystemExit(0)
PY
state_check=$?
set -e
if test "$state_check" = 10; then
  "${COLLECTOR_ZAI:-$(dirname "$0")/collector-zai.py}" \
    --management-key-file "$CLIPROXY_MANAGEMENT_KEY_FILE" \
    --secret-marker-file "$CLIPROXY_MANAGEMENT_KEY_FILE" \
    --secret-marker-file "$ZAI_CODING_PLAN_KEY_FILE" \
    --url "$status_url"
  printf 'dogfood-live: rejected configuration projected as stale config_error\n' >&2
  exit 1
fi
test "$state_check" = 0

python3 - "$status_file" "$CLIPROXY_MANAGEMENT_KEY_FILE" "$ZAI_CODING_PLAN_KEY_FILE" "$CLIPROXY_EXPECTED_PLUGIN_VERSION" <<'PY'
import json, math, os, pathlib, re, stat, sys
expected_top={"plugin","status","version","generated_at","accounts"}
required={"name","key_suffix","plan","five_hour_utilization","weekly_utilization","five_hour_resets_at","weekly_resets_at","quota_source","quota_observed_at","quota_age_seconds","quota_stale","offpeak","health","estimator_complete_since","delivery_warning","persistence_warning","unknown_model_warning","heuristic_dedup_warning","dedup_mode"}
rfc3339=re.compile(r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$")
def fail(message):
    raise SystemExit(message)
def require(condition, message):
    if not condition:
        fail(message)
def read_secret(path):
    # O_NOFOLLOW + fstat on the held descriptor: the bytes read are the bytes whose
    # type, owner, mode, and size were checked, so no pathname substitution applies.
    max_secret_bytes=65536
    fd=os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_CLOEXEC | os.O_NONBLOCK)
    try:
        info=os.fstat(fd)
        if not stat.S_ISREG(info.st_mode):
            fail("secret input file must be a regular file")
        if info.st_uid != os.geteuid():
            fail("secret input file must be owned by the effective user")
        if info.st_mode & 0o077:
            fail("secret input file must not be group- or world-accessible")
        if info.st_size > max_secret_bytes:
            fail("secret input file exceeds the bounded size")
        with os.fdopen(os.dup(fd), "rb") as handle:
            raw=handle.read(max_secret_bytes + 1)
    finally:
        os.close(fd)
    if len(raw) > max_secret_bytes:
        fail("secret input file exceeds the bounded size")
    lines=raw.splitlines()
    if len(lines) != 1 or not lines[0] or lines[0].strip() != lines[0]:
        fail("secret input file must contain exactly one non-empty line")
    try:
        return lines[0].decode("utf-8")
    except UnicodeDecodeError:
        fail("secret input file must contain valid UTF-8")
def scan_decoded(value, markers):
    if isinstance(value, str):
        if any(marker and marker in value for marker in markers):
            fail("authenticated status response failed decoded confidential-value scan")
    elif isinstance(value, dict):
        for key, child in value.items():
            scan_decoded(key, markers)
            scan_decoded(child, markers)
    elif isinstance(value, list):
        for child in value:
            scan_decoded(child, markers)
raw=pathlib.Path(sys.argv[1]).read_bytes()
if len(raw) > 1_048_576:
    fail("authenticated status response exceeds bounded scan size")
management=read_secret(sys.argv[2])
plan=read_secret(sys.argv[3])
markers=[management, plan]
if len(plan) > 6:
    markers.append(plan[-6:])
if any(marker.encode() in raw for marker in markers):
    fail("authenticated status response failed confidential-value scan")
def reject_constant(_):
    raise ValueError("non-RFC JSON value")
try:
    status=json.loads(raw, parse_constant=reject_constant)
except (UnicodeDecodeError, json.JSONDecodeError, ValueError):
    fail("authenticated status response is not strict JSON")
scan_decoded(status, markers)
require(isinstance(status, dict) and set(status) == expected_top, "authenticated status response has unexpected top-level fields")
require(status["plugin"]=="zai-coding-plan" and status["status"]=="registered", "authenticated status response has unexpected plugin state")
require(status["version"]==sys.argv[4], "authenticated status response has unexpected plugin version")
require(isinstance(status["generated_at"], str) and bool(rfc3339.fullmatch(status["generated_at"])), "authenticated status response has invalid generated_at")
require(isinstance(status["accounts"], list) and bool(status["accounts"]), "authenticated status response has no accounts")
for account in status["accounts"]:
    require(isinstance(account, dict), "authenticated status account must be an object")
    require(required <= set(account) and set(account) <= required | {"quota_error"}, "authenticated status account has unexpected fields")
    require(isinstance(account["name"], str) and bool(account["name"]), "authenticated status account has invalid name")
    require(account["key_suffix"] == "redacted", "authenticated status account key suffix is not redacted")
    require(account["quota_source"] in {"quota_api","estimate"}, "authenticated status account has invalid quota source")
    require(isinstance(account["quota_age_seconds"], int) and not isinstance(account["quota_age_seconds"], bool) and account["quota_age_seconds"] >= 0, "authenticated status account has invalid quota age")
    require(all(account[field] is None or isinstance(account[field], str) and rfc3339.fullmatch(account[field]) for field in ("five_hour_resets_at","weekly_resets_at","estimator_complete_since")), "authenticated status account has invalid optional timestamp")
    require(isinstance(account["quota_observed_at"], str) and bool(rfc3339.fullmatch(account["quota_observed_at"])), "authenticated status account has invalid observed timestamp")
    require(all(isinstance(account[field], (int,float)) and not isinstance(account[field], bool) and math.isfinite(account[field]) and 0 <= account[field] <= 1 for field in ("five_hour_utilization","weekly_utilization")), "authenticated status account has invalid utilization")
    require(not any(re.search(r"(?:api[-_]?key|authorization|credential|secret|token|key_hash|identity)", key, re.I) for key in account), "authenticated status account contains a secret-like field")
PY

"${COLLECTOR_ZAI:-$(dirname "$0")/collector-zai.py}" \
  --management-key-file "$CLIPROXY_MANAGEMENT_KEY_FILE" \
  --secret-marker-file "$CLIPROXY_MANAGEMENT_KEY_FILE" \
  --secret-marker-file "$ZAI_CODING_PLAN_KEY_FILE" \
  --url "$status_url"

curl -q --fail-with-body --fail-early --max-redirs 0 --silent --show-error \
  --connect-timeout 2 --max-time 5 --max-filesize 1048576 --noproxy '*' --proxy '' \
  "$CLIPROXY_DASHBOARD_URL" >"$dashboard_file"

timeout --foreground --signal=TERM --kill-after=1 -- "$CLIPROXY_JOURNAL_TIMEOUT" \
  journalctl --unit "$CLIPROXY_SERVICE_UNIT" --since '-15 minutes' --no-pager --output=cat --lines=2000 \
  | python3 -c 'import sys; raw=sys.stdin.buffer.read(1_048_577); sys.stdout.buffer.write(raw); raise SystemExit(len(raw) > 1_048_576)' \
  >"$log_file"

python3 - "$CLIPROXY_USAGE_DIR/zai.json" "$dashboard_file" "$log_file" "$CLIPROXY_MANAGEMENT_KEY_FILE" "$ZAI_CODING_PLAN_KEY_FILE" <<'PY'
import json, os, pathlib, re, stat, sys
projected, dashboard, service_log = map(pathlib.Path, sys.argv[1:4])
def fail(message):
    raise SystemExit(message)
def require(condition, message):
    if not condition:
        fail(message)
def read_secret(path):
    # O_NOFOLLOW + fstat on the held descriptor: the bytes read are the bytes whose
    # type, owner, mode, and size were checked, so no pathname substitution applies.
    max_secret_bytes=65536
    fd=os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_CLOEXEC | os.O_NONBLOCK)
    try:
        info=os.fstat(fd)
        if not stat.S_ISREG(info.st_mode):
            fail("secret input file must be a regular file")
        if info.st_uid != os.geteuid():
            fail("secret input file must be owned by the effective user")
        if info.st_mode & 0o077:
            fail("secret input file must not be group- or world-accessible")
        if info.st_size > max_secret_bytes:
            fail("secret input file exceeds the bounded size")
        with os.fdopen(os.dup(fd), "rb") as handle:
            raw=handle.read(max_secret_bytes + 1)
    finally:
        os.close(fd)
    if len(raw) > max_secret_bytes:
        fail("secret input file exceeds the bounded size")
    lines=raw.splitlines()
    if len(lines) != 1 or not lines[0] or lines[0].strip() != lines[0]:
        fail("secret input file must contain exactly one non-empty line")
    try:
        return lines[0].decode("utf-8")
    except UnicodeDecodeError:
        fail("secret input file must contain valid UTF-8")
def load_json(path):
    try:
        return json.loads(path.read_bytes(), parse_constant=lambda _: fail(f"{path.name} contains a non-RFC JSON value"))
    except (UnicodeDecodeError, json.JSONDecodeError, ValueError):
        fail(f"{path.name} is not strict JSON")
def scan_decoded(value, markers, label):
    if isinstance(value, str):
        if any(marker and marker in value for marker in markers):
            fail(f"{label} failed decoded confidential-value scan")
    elif isinstance(value, dict):
        for key, child in value.items():
            scan_decoded(key, markers, label)
            scan_decoded(child, markers, label)
    elif isinstance(value, list):
        for child in value:
            scan_decoded(child, markers, label)
management=read_secret(sys.argv[4])
plan=read_secret(sys.argv[5])
markers=[management, plan]
if len(plan) > 6:
    markers.append(plan[-6:])
for path in (projected, dashboard, service_log):
    raw = path.read_bytes()
    if len(raw) > 1_048_576:
        fail(f"{path.name} exceeds bounded scan size")
    if any(marker.encode() in raw for marker in markers):
        fail(f"{path.name} failed confidential-value scan")
payload=load_json(projected)
dashboard_payload=load_json(dashboard)
scan_decoded(payload, markers, projected.name)
scan_decoded(dashboard_payload, markers, dashboard.name)
require(isinstance(payload, dict) and payload.get("schemaVersion")==1 and payload.get("lane")=="zai" and isinstance(payload.get("records"), list) and bool(payload["records"]), "projected collector payload has an invalid schema")
serialized=json.dumps(payload, allow_nan=False)
require(not re.search(r'"(?:api[-_]?key|authorization|credential|secret|token|key_hash|identity)"\s*:', serialized, re.I), "projected collector payload contains a secret-like field")
require(all(isinstance(record, dict) and record.get("key_suffix")=="redacted" for record in payload["records"]), "projected collector payload contains an unredacted key suffix")
PY

printf 'dogfood-live: authenticated status, collector lane, dashboard, service-log, and bounded secret-marker scans PASS\n'
