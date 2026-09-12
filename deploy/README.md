# v0.2.0 dogfood deployment

This directory records the exact non-secret inputs and checks for the first Z.ai lane deployment. It does not contain a plan key, management key, host token, or populated config.

## Preconditions proved before host changes

- The tagged v0.2.0 commit, registry bytes, and release checksums are recorded by the release artifact manifest; do not substitute a working-tree or untagged artifact.
- The compatibility evidence records deployed, latest, and v7.2.67 baseline image digests and successful host checks.
- `config.yaml.tmpl` follows `docs/ARCHITECTURE.md`: full-key pairing is rendered only on the host; `zai-coding-plan` is the sole enabled scheduler at priority `1000`.

The release operator bundle is the source of truth for the exact commit, registry digest, compatibility evidence, live verifier, rollback script, and per-file modes. Verify its manifest and `checksums.txt` before using any command below.

### Release preflight

```sh
bundle=/path/to/zai-coding-plan-v0.2.0-operator.zip
checksums=/path/to/checksums.txt
sha256sum --check "$checksums"
unzip -Z1 "$bundle" | sort
```

Each bundle entry must match the strict manifest exactly; extra, duplicate, traversal, or unresolved-template entries are invalid.

- The plugin-store registry source is pinned to the immutable release commit recorded in the bundle manifest.
- `checksums.txt` covers the shared library, plugin-store archive, operator bundle, compatibility evidence, live verifier, rollback script, and corrected templates.

Run the credential-free checks before opening the operator handoff:

```sh
./deploy/acceptance_local.py
```

Verify the exact registry bytes before merging the template into the host config:

```sh
registry=$(mktemp)
trap 'rm -f "$registry"' EXIT
curl --fail-with-body --fail-early --max-redirs 0 --silent --show-error \
  "https://raw.githubusercontent.com/TogetherWeOwn/cpa-plugin-zai-coding-plan/$RELEASE_SHA/registry.json" \
  >"$registry"
printf '%s  %s\n' "$REGISTRY_SHA256" "$registry" | sha256sum --check --status
```

## Exact host install and validation

The operator card must substitute the deployment's real container/config paths, but these commands are the invariant core. The management key and Z.ai key are read from root-only files; neither is placed in an argument, output, or board comment. Curl reads the Authorization header from a root-only config file, so the credential is absent from process argv.

```sh
set -euo pipefail
umask 077
repo=/home/ubuntu/cpa-plugin-zai-coding-plan
config=/home/ubuntu/cliproxy/config.yaml
management_key_file=/home/ubuntu/secure-drop/cliproxy-management.key
plan_key_file=/home/ubuntu/secure-drop/zai-coding-plan.key
backup="${config}.pre-zai-$(date -u +%Y%m%dT%H%M%SZ)"
cp -a "$config" "$backup"
curl_config=$(mktemp)
trap 'rm -f "$curl_config"' EXIT
python3 - "$management_key_file" "$curl_config" <<'PY'
import pathlib, sys
key_path=pathlib.Path(sys.argv[1])
if key_path.is_symlink() or key_path.stat().st_mode & 0o777 != 0o600:
    raise SystemExit("management key file must be mode 0600 and not a symlink")
key=key_path.read_text().strip()
if not key or "\n" in key or "\r" in key:
    raise SystemExit("management key file must contain one non-empty line")
path=pathlib.Path(sys.argv[2])
path.write_text('header = "Authorization: Bearer ' + key.replace('\\', '\\\\').replace('"', '\\"') + '"\n')
path.chmod(0o600)
PY

# Render the complete candidate config without printing credential values, then replace
# config.yaml durably from the same directory. Never rename across filesystems.
config_dir=$(dirname "$config")
test ! -L "$config_dir"
test "$(stat -c %U:%G "$config_dir")" = root:root
test "$((8#$(stat -c %a "$config_dir") & 8#077))" = 0
candidate=$(mktemp --tmpdir="$config_dir" .config.yaml.zai.XXXXXX)
trap 'rm -f "$curl_config" "$candidate"' EXIT
# Merge deploy/config.yaml.tmpl into "$candidate" without logging rendered secrets.
chmod 0600 "$candidate"
python3 - "$candidate" "$config_dir" <<'PY'
import os, pathlib, sys
candidate=pathlib.Path(sys.argv[1])
directory=pathlib.Path(sys.argv[2])
with candidate.open("rb") as handle:
    os.fsync(handle.fileno())
directory_fd=os.open(directory, os.O_RDONLY | os.O_DIRECTORY)
try:
    os.fsync(directory_fd)
finally:
    os.close(directory_fd)
PY
mv -T "$candidate" "$config"
candidate=
python3 - "$config_dir" <<'PY'
import os, pathlib, sys
directory_fd=os.open(pathlib.Path(sys.argv[1]), os.O_RDONLY | os.O_DIRECTORY)
try:
    os.fsync(directory_fd)
finally:
    os.close(directory_fd)
PY
test "$(stat -c %a "$config")" = 600
systemctl reload cliproxy.service || systemctl restart cliproxy.service

# After config reload exposes the custom source, install the exact release. Keep the
# response body in a root-only bounded file so an error page never reaches operator output.
install_response=$(mktemp)
install_error=$(mktemp)
trap 'rm -f "$curl_config" "$install_response" "$install_error"' EXIT
if curl --fail --fail-early --max-redirs 0 --silent --show-error \
  --connect-timeout 2 --max-time 10 --max-filesize 1048576 \
  --config "$curl_config" \
  --output "$install_response" --stderr "$install_error" \
  -X POST \
  -H 'Content-Type: application/json' \
  --data '{"version":"0.2.0"}' \
  'http://127.0.0.1:8317/v0/management/plugin-store/zai-coding-plan/install'
then
  :
else
  rc=$?
  printf 'plugin install request failed (curl exit %s; response body suppressed)\n' "$rc" >&2
  if grep -Eq '^curl: \([0-9]+\) (Connection|Could not|Failed|Operation timed out|Maximum file size exceeded|Received HTTP code|The requested URL returned error)[[:print:]]{0,240}$' "$install_error"; then
    tr -d '\r\n' <"$install_error" >&2
    printf '\n' >&2
  fi
  exit "$rc"
fi
python3 - "$install_response" <<'PY'
import json, pathlib, sys
path=pathlib.Path(sys.argv[1])
if path.stat().st_size > 1_048_576:
    raise SystemExit("plugin install response exceeded 1 MiB")
try:
    response=json.loads(path.read_text())
except (OSError, UnicodeError, json.JSONDecodeError):
    raise SystemExit("plugin install response was not valid JSON")
expected={"id":"zai-coding-plan","version":"0.2.0","install_type":"github-release"}
if any(response.get(key) != value for key, value in expected.items()):
    raise SystemExit("plugin install response did not confirm the expected release")
path_value=response.get("path")
if not isinstance(path_value, str) or "/linux/amd64/" not in path_value or "0.2.0" not in path_value:
    raise SystemExit("plugin install response did not report the expected versioned linux/amd64 path")
PY

usage_dir=/srv/cliproxy-usage
# This helper accepts only the fixed usage path, opens every ancestor with
# O_NOFOLLOW, then verifies the pathname still names the secured directory fd.
python3 "$repo/deploy/prepare-usage-dir.py" "$usage_dir"
test "$(stat -c %u:%g:%a "$usage_dir")" = 0:0:700
CLIPROXY_MANAGEMENT_KEY_FILE="$management_key_file" \
ZAI_CODING_PLAN_KEY_FILE="$plan_key_file" \
CLIPROXY_USAGE_DIR="$usage_dir" \
CLIPROXY_DASHBOARD_URL=http://127.0.0.1:3000/api/telemetry/model-usage/zai \
CLIPROXY_SERVICE_UNIT=cliproxy.service \
  "$repo/deploy/verify-live.sh"
```

The plugin-store response must report `id=zai-coding-plan`, `version=0.2.0`, `install_type=github-release`, and a versioned `linux/amd64` path. The host installer verifies the release `checksums.txt`; `deploy/verify-live.sh` then proves authenticated status field names, writes sanitized `/srv/cliproxy-usage/zai.json`, and performs bounded projected-output, dashboard, and service-log scans for both management-key and plan-key markers without printing matches.

`router-capacity-source.json` is the exact Model Router capacity-source shape for one opaque Z.ai model ID. Repeat it per model ID and retain `unknownTelemetry: fail-open` during dogfood. The operator dispatcher already maps `zai/*`, `zai-openai/*`, and `glm*` to lane `zai`; the live check is a dry-run selection with the Z.ai model enabled, followed by one bounded canary issue. Do not re-pin an issue mid-run.

## Rollback

```sh
set -euo pipefail
umask 077
repo=/home/ubuntu/cpa-plugin-zai-coding-plan
config=/home/ubuntu/cliproxy/config.yaml
backup=/home/ubuntu/cliproxy/config.yaml.pre-zai-YYYYMMDDTHHMMSSZ # use the recorded install backup
test -f "$backup" && test ! -L "$backup"
test "$(stat -c %a "$backup")" = 600
config_dir=$(dirname "$config")
test ! -L "$config_dir"
test "$(stat -c %U:%G "$config_dir")" = root:root
test "$((8#$(stat -c %a "$config_dir") & 8#077))" = 0
management_key_file=/home/ubuntu/secure-drop/cliproxy-management.key
curl_config=$(mktemp)
delete_response=$(mktemp)
delete_error=$(mktemp)
trap 'rm -f "$curl_config" "$delete_response" "$delete_error"' EXIT
python3 - "$management_key_file" "$curl_config" <<'PY'
import pathlib, sys
key_path=pathlib.Path(sys.argv[1])
if key_path.is_symlink() or key_path.stat().st_mode & 0o777 != 0o600:
    raise SystemExit("management key file must be mode 0600 and not a symlink")
key=key_path.read_text().strip()
if not key or "\n" in key or "\r" in key:
    raise SystemExit("management key file must contain one non-empty line")
path=pathlib.Path(sys.argv[2])
path.write_text('header = "Authorization: Bearer ' + key.replace('\\', '\\\\').replace('"', '\\"') + '"\n')
path.chmod(0o600)
PY
config_dir=$(dirname "$config")
test ! -L "$config_dir"
test "$(stat -c %U:%G "$config_dir")" = root:root
restored=$(mktemp --tmpdir="$config_dir" .config.yaml.rollback.XXXXXX)
trap 'rm -f "$curl_config" "$restored"' EXIT
install -m 0600 "$backup" "$restored"
python3 - "$restored" <<'PY'
import os, pathlib, sys
with pathlib.Path(sys.argv[1]).open("rb") as handle:
    os.fsync(handle.fileno())
PY
mv -T "$restored" "$config"
restored=
python3 - "$config_dir" <<'PY'
import os, pathlib, sys
directory_fd=os.open(pathlib.Path(sys.argv[1]), os.O_RDONLY | os.O_DIRECTORY)
try:
    os.fsync(directory_fd)
finally:
    os.close(directory_fd)
PY
if curl --fail --fail-early --max-redirs 0 --silent --show-error \
  --connect-timeout 2 --max-time 5 --max-filesize 1048576 \
  --config "$curl_config" \
  --output "$delete_response" --stderr "$delete_error" \
  -X DELETE \
  'http://127.0.0.1:8317/v0/management/plugins/zai-coding-plan'
then
  :
else
  if grep -Eq '^curl: \([0-9]+\) [[:print:]]{0,240}$' "$delete_error"; then
    tr -d '\r\n' <"$delete_error" >&2
    printf '\n' >&2
  fi
  printf 'management unavailable; configuration restore remains recoverable; plugin cleanup is pending (response body suppressed)\n' >&2
fi
systemctl reload cliproxy.service || systemctl restart cliproxy.service
python3 "$repo/deploy/remove-usage-output.py"
```

The rollback is complete only after the restored configuration has been loaded by the running service. If reload is unsupported or fails, the command above restarts the existing CLIProxy service rather than leaving the pre-rollback snapshot active.

## Evidence and redaction

- Synthetic 401, 403, 429, threshold exhaustion, paired-sibling exclusion, healthy native round-robin, and collector redaction are executed by `acceptance_local.py`.
- Live validation stores response bodies only in mode-`0600` temporary files and checks field names; commands and comments record status codes and hashes, never key material.
- The collector rejects redirects, restricts management requests to loopback or an explicit `--allowed-origin`, caps the authenticated response at 1 MiB, requires exact schemas, RFC 3339 timestamps, integer quota ages, finite RFC JSON numbers, and one-line key files, and redacts secret-shaped string values.
- A `reconfigure_rejected` snapshot is retained only as unavailable capacity: every projected account becomes `config_error` and stale, while live installation verification fails that state.
- The collector requires an absolute output under a verified root-owned real directory, rejects symlinked or substituted parents, enforces mode `0700` on the directory and `0600` on initial and replacement files, fsyncs file data, atomically renames relative to the held directory descriptor, then fsyncs that directory.
