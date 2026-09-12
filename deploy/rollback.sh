#!/usr/bin/env bash
set -euo pipefail

: "${CLIPROXY_MANAGEMENT_URL:=http://127.0.0.1:8317}"
: "${CLIPROXY_CONFIG:=/home/ubuntu/cliproxy/config.yaml}"
: "${CLIPROXY_BACKUP:?set CLIPROXY_BACKUP to the recorded pre-install backup}"
: "${CLIPROXY_MANAGEMENT_KEY_FILE:?set CLIPROXY_MANAGEMENT_KEY_FILE to a root-readable 0600 file}"
: "${CLIPROXY_USAGE_DIR:=/srv/cliproxy-usage}"

umask 077
management_url="${CLIPROXY_MANAGEMENT_URL%/}/v0/management/plugins/zai-coding-plan"
python3 - "$CLIPROXY_MANAGEMENT_URL" "$management_url" <<'PY'
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
base, removal=sys.argv[1:]
if origin(base) not in {origin(value) for value in allowed}:
    raise SystemExit("management URL origin is not approved")
parsed=urllib.parse.urlsplit(removal)
if parsed.path != "/v0/management/plugins/zai-coding-plan" or origin(removal) != origin(base):
    raise SystemExit("management plugin removal URL is not approved")
PY

config_dir=$(dirname -- "$CLIPROXY_CONFIG")
test -d "$config_dir" && test ! -L "$config_dir"
test "$(stat -c '%u:%g:%a' -- "$config_dir")" = '0:0:700' || {
  printf '%s\n' 'config directory must be root-owned with mode 0700' >&2
  exit 1
}
test -f "$CLIPROXY_BACKUP" && test ! -L "$CLIPROXY_BACKUP"
test "$(stat -c '%u:%a' -- "$CLIPROXY_BACKUP")" = '0:600' || {
  printf '%s\n' 'rollback backup must be root-owned with mode 0600' >&2
  exit 1
}

restored=$(mktemp --tmpdir="$config_dir" .config.yaml.rollback.XXXXXX)
cleanup() { rm -f -- "$restored" "$curl_config"; }
trap cleanup EXIT
install -m 0600 -- "$CLIPROXY_BACKUP" "$restored"
python3 - "$restored" "$config_dir" <<'PY'
import os, pathlib, sys
with pathlib.Path(sys.argv[1]).open('rb') as handle:
    os.fsync(handle.fileno())
directory_fd=os.open(sys.argv[2], os.O_RDONLY | os.O_DIRECTORY)
try:
    os.fsync(directory_fd)
finally:
    os.close(directory_fd)
PY
mv -T -- "$restored" "$CLIPROXY_CONFIG"
restored=

curl_config=$(mktemp)
python3 - "$CLIPROXY_MANAGEMENT_KEY_FILE" "$curl_config" <<'PY'
import os, pathlib, sys
key_path=pathlib.Path(sys.argv[1])
if key_path.is_symlink() or key_path.stat().st_uid != 0 or key_path.stat().st_mode & 0o777 != 0o600:
    raise SystemExit('management key file must be root-owned mode 0600 and not a symlink')
key=key_path.read_text().strip()
if not key or '\n' in key or '\r' in key:
    raise SystemExit('management key file must contain one non-empty line')
path=pathlib.Path(sys.argv[2])
path.write_text('header = "Authorization: Bearer ' + key.replace('\\', '\\\\').replace('"', '\\"') + '"\n')
path.chmod(0o600)
PY
if curl -q --fail --fail-early --max-redirs 0 --silent --show-error --connect-timeout 2 --max-time 5 --max-filesize 1048576 --noproxy '*' --proxy '' --config "$curl_config" -X DELETE "$management_url" >/dev/null; then
  printf '%s\n' 'management-plane plugin removal PASS'
else
  printf '%s\n' 'management unavailable; configuration restore completed, plugin cleanup remains pending' >&2
fi
systemctl reload cliproxy.service || systemctl restart cliproxy.service
if test -f "$(dirname "$0")/remove-usage-output.py"; then
  python3 "$(dirname "$0")/remove-usage-output.py" "$CLIPROXY_USAGE_DIR"
fi
printf '%s\n' 'rollback: restored configuration and reloaded service'
