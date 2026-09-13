#!/usr/bin/env bash
set -euo pipefail

: "${CLIPROXY_MANAGEMENT_URL:=http://127.0.0.1:8317}"
: "${CLIPROXY_USAGE_DIR:=/srv/cliproxy-usage}"
: "${CLIPROXY_MANAGEMENT_KEY_FILE:?set CLIPROXY_MANAGEMENT_KEY_FILE to a root-readable 0600 file}"
: "${OPENCODE_GO_DASHBOARD_API_KEY_FILE:?set OPENCODE_GO_DASHBOARD_API_KEY_FILE to a root-readable 0600 file}"
: "${CLIPROXY_SERVICE_UNIT:=cliproxy.service}"
: "${CLIPROXY_JOURNAL_TIMEOUT:=5}"

require_secure_key_file() {
  local path=$1
  test -f "$path" || { printf '%s\n' "key file must be a regular file" >&2; exit 1; }
  test ! -L "$path" || { printf '%s\n' "key file must not be a symlink" >&2; exit 1; }
  test "$(stat -c '%a' -- "$path")" = "600" || { printf '%s\n' "key file must have mode 0600" >&2; exit 1; }
}
require_secure_key_file "$CLIPROXY_MANAGEMENT_KEY_FILE"
require_secure_key_file "$OPENCODE_GO_DASHBOARD_API_KEY_FILE"

python3 - "$CLIPROXY_JOURNAL_TIMEOUT" <<'PY'
import re, sys
for name, value in zip(("CLIPROXY_JOURNAL_TIMEOUT",), sys.argv[1:]):
    if len(value) > 16 or not re.fullmatch(r"(?:[1-9]\d*(?:\.\d+)?|0\.\d*[1-9]\d*)[smh]?", value):
        raise SystemExit(f"{name} must be a positive duration using s, m, or h")
PY

umask 077
work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT

status_file="$work_dir/status.json"
log_file="$work_dir/service.log"
curl_config="$work_dir/management.curl"
status_url="${CLIPROXY_MANAGEMENT_URL%/}/v0/management/plugins/subscription-pool/status"

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
if parsed.path != "/v0/management/plugins/subscription-pool/status" or origin(status) != origin(base):
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

unauthenticated=$(curl -q --fail-early --max-redirs 0 --silent --show-error --max-time 5 --max-filesize 1048576 --noproxy '*' --proxy '' --output /dev/null --write-out '%{http_code}' "$status_url")
test "$unauthenticated" = 401

curl -q --fail-with-body --fail-early --max-redirs 0 --silent --show-error \
  --max-time 5 --max-filesize 1048576 --noproxy '*' --proxy '' \
  --config "$curl_config" \
  "$status_url" >"$status_file"

python3 - "$status_file" "$CLIPROXY_MANAGEMENT_KEY_FILE" "$OPENCODE_GO_DASHBOARD_API_KEY_FILE" <<'PY'
import json, math, pathlib, re, sys
coordinator_expected_top={"plugin","status","version","generated_at","providers","validation_error"}
coordinator_required_top={"plugin","status","version","generated_at","providers"}
provider_expected={"provider","status","validation_error","credential_bound","accounts","observation_gaps"}
provider_required={"provider","status","credential_bound","observation_gaps"}
account_expected={"name","disabled","windows"}
account_required={"name","windows"}
window_kinds={"five_hour","weekly","monthly"}
window_expected={"known","exhausted","utilization","resets_at","source","authoritative"}
window_required={"known","exhausted"}
rfc3339=re.compile(r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$")
def fail(message):
    raise SystemExit(message)
def require(condition, message):
    if not condition:
        fail(message)
def read_secret(path):
    raw=pathlib.Path(path).read_bytes()
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
dashboard_key=read_secret(sys.argv[3])
markers=[management, dashboard_key]
if len(dashboard_key) > 6:
    markers.append(dashboard_key[-6:])
if any(marker.encode() in raw for marker in markers):
    fail("authenticated status response failed confidential-value scan")
def reject_constant(_):
    raise ValueError("non-RFC JSON value")
try:
    status=json.loads(raw, parse_constant=reject_constant)
except (UnicodeDecodeError, json.JSONDecodeError, ValueError):
    fail("authenticated status response is not strict JSON")
scan_decoded(status, markers)
require(isinstance(status, dict) and coordinator_required_top <= set(status) <= coordinator_expected_top, "authenticated status response has unexpected top-level fields")
require(status["plugin"]=="subscription-pool" and status["status"]=="registered", "authenticated status response has unexpected plugin state")
require(isinstance(status["version"], str) and bool(status["version"]), "authenticated status response has invalid version")
require(isinstance(status["generated_at"], str) and bool(rfc3339.fullmatch(status["generated_at"])), "authenticated status response has invalid generated_at")
providers=status["providers"]
require(isinstance(providers, dict) and "opencode-go" in providers, "authenticated status response is missing the opencode-go provider entry")
provider=providers["opencode-go"]
require(isinstance(provider, dict), "authenticated opencode-go provider status must be an object")
require(provider_required <= set(provider) <= provider_expected, "authenticated opencode-go provider status has unexpected fields")
require(provider["provider"]=="opencode-go" and provider["status"]=="registered", "authenticated opencode-go provider has unexpected status")
require(isinstance(provider["credential_bound"], bool), "authenticated opencode-go provider has invalid credential_bound")
require(provider["credential_bound"] is True, "authenticated opencode-go provider has no bound dashboard credential")
gaps=provider["observation_gaps"]
require(isinstance(gaps, list) and all(isinstance(gap, str) for gap in gaps), "authenticated opencode-go provider has invalid observation_gaps")
accounts=provider.get("accounts", [])
require(isinstance(accounts, list) and bool(accounts), "authenticated opencode-go provider has no accounts")
require(any(isinstance(account, dict) and not account.get("disabled", False) for account in accounts), "authenticated opencode-go provider has no usable managed capacity")
for account in accounts:
    require(isinstance(account, dict), "authenticated opencode-go account must be an object")
    require(account_required <= set(account) <= account_expected, "authenticated opencode-go account has unexpected fields")
    require(isinstance(account["name"], str) and bool(account["name"]), "authenticated opencode-go account has invalid name")
    windows=account["windows"]
    require(isinstance(windows, dict) and set(windows) == window_kinds, "authenticated opencode-go account windows must contain exactly five_hour, weekly, and monthly")
    for kind, window in windows.items():
        require(isinstance(window, dict), f"authenticated opencode-go {kind} window must be an object")
        require(window_required <= set(window) <= window_expected, f"authenticated opencode-go {kind} window has unexpected fields")
        require(isinstance(window["known"], bool) and isinstance(window["exhausted"], bool), f"authenticated opencode-go {kind} window has invalid known/exhausted")
        if not window["known"]:
            require("utilization" not in window and "resets_at" not in window, f"authenticated opencode-go {kind} window reports data while unknown")
        else:
            if "utilization" in window:
                value=window["utilization"]
                require(isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value) and 0 <= value <= 1, f"authenticated opencode-go {kind} window has invalid utilization")
            if "resets_at" in window:
                require(isinstance(window["resets_at"], str) and bool(rfc3339.fullmatch(window["resets_at"])), f"authenticated opencode-go {kind} window has invalid resets_at")
    require(not any(re.search(r"(?:api[-_]?key|authorization|credential|secret|token|key_hash|identity)", key, re.I) and key != "credential_bound" for key in account), "authenticated opencode-go account contains a secret-like field")
PY

"${COLLECTOR_OPENCODEGO:-$(dirname "$0")/collector-opencodego.py}" \
  --management-key-file "$CLIPROXY_MANAGEMENT_KEY_FILE" \
  --secret-marker-file "$CLIPROXY_MANAGEMENT_KEY_FILE" \
  --secret-marker-file "$OPENCODE_GO_DASHBOARD_API_KEY_FILE" \
  --url "$status_url"

timeout --foreground --signal=TERM --kill-after=1 -- "$CLIPROXY_JOURNAL_TIMEOUT" \
  journalctl --unit "$CLIPROXY_SERVICE_UNIT" --since '-15 minutes' --no-pager --output=cat --lines=2000 \
  | python3 -c 'import sys; raw=sys.stdin.buffer.read(1_048_577); sys.stdout.buffer.write(raw); raise SystemExit(len(raw) > 1_048_576)' \
  >"$log_file"

python3 - "$CLIPROXY_USAGE_DIR/opencode-go.json" "$log_file" "$CLIPROXY_MANAGEMENT_KEY_FILE" "$OPENCODE_GO_DASHBOARD_API_KEY_FILE" <<'PY'
import json, pathlib, re, sys
projected, service_log = map(pathlib.Path, sys.argv[1:3])
def fail(message):
    raise SystemExit(message)
def require(condition, message):
    if not condition:
        fail(message)
def read_secret(path):
    raw=pathlib.Path(path).read_bytes()
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
management=read_secret(sys.argv[3])
dashboard_key=read_secret(sys.argv[4])
markers=[management, dashboard_key]
if len(dashboard_key) > 6:
    markers.append(dashboard_key[-6:])
for path in (projected, service_log):
    raw = path.read_bytes()
    if len(raw) > 1_048_576:
        fail(f"{path.name} exceeds bounded scan size")
    if any(marker.encode() in raw for marker in markers):
        fail(f"{path.name} failed confidential-value scan")
payload=load_json(projected)
scan_decoded(payload, markers, projected.name)
require(isinstance(payload, dict) and payload.get("schemaVersion")==1 and payload.get("lane")=="opencode-go" and isinstance(payload.get("records"), list) and bool(payload["records"]), "projected collector payload has an invalid schema")
serialized=json.dumps(payload, allow_nan=False)
require(not re.search(r'"(?:api[-_]?key|authorization|credential|secret|token|key_hash|identity)"\s*:', serialized, re.I), "projected collector payload contains a secret-like field")
require(payload.get("credential_bound") is True, "projected collector payload reports no bound dashboard credential")
PY

printf 'dogfood-live: authenticated coordinator status, opencode-go provider lane, collector output, and service-log confidential-value scans PASS\n'
