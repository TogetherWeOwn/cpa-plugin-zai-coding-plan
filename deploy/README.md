# v0.4.5 subscription-pool deployment

This directory records the exact non-secret inputs and checks for the Z.ai lane deployment of the neutral `subscription-pool` coordinator. It does not contain a plan key, management key, host token, or populated config.

## Preconditions proved before host changes

- The tagged v0.4.5 commit, registry bytes, and release checksums are recorded by the release artifact manifest; do not substitute a working-tree or untagged artifact.
- The compatibility evidence records deployed, latest, and v7.2.150 baseline image digests and successful host checks.
- `config.yaml.tmpl` follows `docs/ARCHITECTURE.md`: full-key pairing is rendered only on the host; `subscription-pool` is the sole enabled scheduler at priority `1000`.

The release operator bundle is the source of truth for the exact commit, registry digest, compatibility evidence, live verifier, rollback script, and per-file modes. Verify its manifest and `checksums.txt` before using any command below.

### Current-host Docker acceptance (v0.4.5 packet)

The live host is a Docker stack, not systemd: host config
`/home/ubuntu/stacks/cliproxy/config.yaml` is bind-mounted to
`/CLIProxyAPI/config.yaml`, and the live plugin directory is
`/home/ubuntu/stacks/cliproxy/plugins/linux/amd64/`. OmniRoute is retired and
the historical model-usage dashboard endpoint is retired; do not restart
either, recreate `cliproxy.service`, or change host ownership to satisfy an
old recipe. The host's usage-snapshot service is the sole quota poller and
feed writer; verifiers run snapshot-only and never invoke collectors against
live feeds.

```sh
set -euo pipefail
umask 077
# Credential source: the established host file holds CLIPROXY_MGMT_KEY.
# Extract only that variable into a root-only temp key file, in process;
# never print it, export it, or copy it to a new lasting file.
management_key_file=$(mktemp)
trap 'rm -f "$management_key_file"' EXIT
python3 - /home/ubuntu/secure-drop/cliproxy.env "$management_key_file" <<'PY'
import pathlib, shlex, sys
value = None
for line in pathlib.Path(sys.argv[1]).read_text().splitlines():
    line = line.strip()
    if not line or line.startswith("#") or "=" not in line:
        continue
    key, _, raw = line.partition("=")
    if key.strip() == "CLIPROXY_MGMT_KEY":
        parts = shlex.split(raw.strip(), posix=True)
        value = parts[0] if parts else ""
        break
if not value:
    raise SystemExit("CLIPROXY_MGMT_KEY not found in established credential source")
path = pathlib.Path(sys.argv[2])
path.write_text(value + "\n")
path.chmod(0o600)
PY
plan_key_file=<existing host Z.ai plan-key file>
# Discover the running container name on the host; record it, do not guess.
docker ps --format '{{.Names}} {{.Image}}' | grep -i cliproxy
container=<cliproxy-container-from-docker-ps>
usage_dir=/srv/cliproxy-usage
CLIPROXY_MANAGEMENT_KEY_FILE="$management_key_file" \
ZAI_CODING_PLAN_KEY_FILE="$plan_key_file" \
CLIPROXY_USAGE_DIR="$usage_dir" \
CLIPROXY_CONTAINER="$container" \
CLIPROXY_SNAPSHOT_ONLY=1 \
  ./verify-live.sh
```

The Go lane needs `OPENCODE_GO_DASHBOARD_API_KEY_FILE` only when the lane is
provisioned. When the host has no bound Go dashboard credential, pass
`OPENCODE_GO_ALLOW_UNBOUND=1` with `CLIPROXY_SNAPSHOT_ONLY=1`; the verifier
then records `UNAVAILABLE (no bound dashboard credential; unchanged, not a
v0.4.5 regression)` after the same confidential-value scans, and never
fabricates capacity. Do not create accounts, copy new credentials, or change
lane policy to manufacture a PASS.

Rollback on this host preserves the existing v7-layout config and ownership
(no chown): set `CLIPROXY_CONFIG` (default
`/home/ubuntu/stacks/cliproxy/config.yaml`),
`CLIPROXY_CONFIG_UID`/`CLIPROXY_CONFIG_GID` to the actual config owner when it
is not root, and `CLIPROXY_CONTAINER` so the service reload becomes
`docker restart`. The usage-output cleanup targets the fixed feed path only
when the usage directory exists.

Status-shape note: v0.4.5 emits `identity` and `cooldown` on every Z.ai
account (see `docs/STATUS-CONTRACT.md`) plus an optional `five_hour_error`.
The collector and both live verifiers accept exactly that shape with strict
64-hex identity, closed-vocabulary cooldown, and unchanged secret scans; older
script copies reject live v0.4.5 status and must not be used for acceptance.

Compatibility note: v0.4.5 builds against SDK v7.2.67 (ABI 1, schema 1),
identical to the live v0.4.4, with no new RPC surface; the v0.4.4 library is
loaded and serving on the current patched host image today. Release CI passed
the exact-image matrix on stock v7.2.151 (deployed evidence), v8.0.13
(latest), and v7.2.67 (baseline). The patched
`v8.0.12-tog.3-musereplay` host image itself was never in CI; that is an
evidence gap, not proof of incompatibility. The bounded host preflight is the
management plugin-list registration check plus authenticated status on the new
library, with no inference probe.

### Release preflight

```sh
bundle=/path/to/subscription-pool-v0.4.5-operator.zip
checksums=/path/to/checksums.txt
sha256sum --check "$checksums"
unzip -Z1 "$bundle" | sort
```

Each bundle entry must match the strict manifest exactly; extra, duplicate, traversal, or unresolved-template entries are invalid.

- The plugin-store registry source is pinned to the immutable release commit recorded in the bundle manifest.
- `checksums.txt` covers every published file: the shared library, plugin-store archive, operator bundle, registry, compatibility evidence, live verifier, rollback script, helper scripts, provenance, runbook, and corrected templates.

Run the credential-free checks before opening the operator handoff:

```sh
./deploy/acceptance_local.py
```

Verify the exact registry bytes before merging the template into the host config:

```sh
registry=$(mktemp)
trap 'rm -f "$registry"' EXIT
release_dir=/path/to/downloaded-v0.4.5-release-assets
cp "$release_dir/registry.json" "$registry"
grep -F "  registry.json" "$release_dir/checksums.txt" \
  | sed "s#  registry.json#  $registry#" \
  | sha256sum --check --status
release_sha=$(<"$release_dir/release-sha.txt")
test "$release_sha" = "$RELEASE_SHA"
```

## Exact host install and validation

The operator card must substitute the deployment's real container/config paths, but these commands are the invariant core. The management key and Z.ai key are read from root-only files; neither is placed in an argument, output, or board comment. Curl reads the Authorization header from a root-only config file, so the credential is absent from process argv.

```sh
set -euo pipefail
umask 077
repo=/home/ubuntu/cpa-plugin-zai-coding-plan
config=/home/ubuntu/stacks/cliproxy/config.yaml
management_key_file=/home/ubuntu/secure-drop/cliproxy-management.key
plan_key_file=/home/ubuntu/secure-drop/zai-coding-plan.key
container=<cliproxy-container-from-docker-ps>
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
docker restart "$container"

# After the container reload exposes the custom source, install the exact release. Keep the
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
  --data '{"version":"0.4.5"}' \
  'http://127.0.0.1:8317/v0/management/plugin-store/subscription-pool/install'
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
expected={"id":"subscription-pool","version":"0.4.5","install_type":"github-release"}
if any(response.get(key) != value for key, value in expected.items()):
    raise SystemExit("plugin install response did not confirm the expected release")
path_value=response.get("path")
if not isinstance(path_value, str) or "/linux/amd64/" not in path_value or "0.4.5" not in path_value:
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
CLIPROXY_CONTAINER="$container" \
CLIPROXY_SNAPSHOT_ONLY=1 \
  "$repo/deploy/verify-live.sh"
```

The plugin-store response must report `id=subscription-pool`, `version=0.4.5`, `install_type=github-release`, and a versioned `linux/amd64` path. The host installer verifies the release `checksums.txt`; `deploy/verify-live.sh` then proves authenticated status field names (including the v0.4.5 `identity`/`cooldown` shape), reads the snapshot service's sanitized `/srv/cliproxy-usage/zai.json` with a freshness bound, and performs bounded projected-output and Docker service-log scans for both management-key and plan-key markers without printing matches. The historical dashboard fetch runs only when `CLIPROXY_DASHBOARD_URL` is explicitly set; the retired endpoint is skipped by default.

`router-capacity-source.json` is the exact Model Router capacity-source shape for one opaque Z.ai model ID. Repeat it per model ID and retain `unknownTelemetry: fail-open` during dogfood. The operator dispatcher already maps `zai/*`, `zai-openai/*`, and `glm*` to lane `zai`; the live check is a dry-run selection with the Z.ai model enabled, followed by one bounded canary issue. Do not re-pin an issue mid-run.

## OpenCode Go lane collector and live check

The OpenCode Go provider module has no dedicated management route of its own; it is aggregated under the coordinator's own status route, keyed by the module's `ID()` (`opencode-go`):

```
GET /v0/management/plugins/subscription-pool/status
-> {"plugin":"subscription-pool", ..., "providers": {"opencode-go": {...}, "zai": {...}}}
```

`deploy/collector-opencodego.py` polls that same aggregate route, extracts the `providers["opencode-go"]` entry, validates it against the module's actual `Status()` field shape (`provider`, `status`, `credential_bound`, `observation_gaps`, `accounts[].name/disabled/windows{five_hour,weekly,monthly}.known/utilization/exhausted/resets_at/source/authoritative`), and writes sanitized `/srv/cliproxy-usage/opencode-go.json`. It fails closed if the `opencode-go` entry is absent from `providers` (a transient module error inside the coordinator's own aggregation loop silently drops the key rather than propagating an error).

```sh
usage_dir=/srv/cliproxy-usage
CLIPROXY_MANAGEMENT_KEY_FILE="$management_key_file" \
OPENCODE_GO_DASHBOARD_API_KEY_FILE=/home/ubuntu/secure-drop/opencode-go-dashboard.key \
CLIPROXY_USAGE_DIR="$usage_dir" \
CLIPROXY_CONTAINER="$container" \
CLIPROXY_SNAPSHOT_ONLY=1 \
  "$repo/deploy/verify-live-opencodego.sh"
```

`deploy/verify-live-opencodego.sh` is the near-direct adaptation of `deploy/verify-live.sh` for this lane: it proves the same unauthenticated-401/authenticated-200 shape against the coordinator's aggregate route, validates the outer coordinator envelope loosely and the inner `providers["opencode-go"]` object strictly, reads the snapshot service's feed with a freshness bound in snapshot-only mode, and performs bounded projected-output and Docker service-log confidential-value scans for both the management key and the OpenCode Go dashboard API key markers without printing matches. It has no dashboard-fetch step. When the lane has no bound dashboard credential, `OPENCODE_GO_ALLOW_UNBOUND=1` records an explicit `UNAVAILABLE` disposition instead of manufacturing a PASS; the strict default still requires bound usable capacity.

## Rollback

```sh
set -euo pipefail
umask 077
repo=/home/ubuntu/cpa-plugin-zai-coding-plan
config=/home/ubuntu/stacks/cliproxy/config.yaml
backup=/home/ubuntu/stacks/cliproxy/config.yaml.pre-zai-YYYYMMDDTHHMMSSZ # use the recorded install backup
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
  'http://127.0.0.1:8317/v0/management/plugins/subscription-pool'
then
  :
else
  if grep -Eq '^curl: \([0-9]+\) [[:print:]]{0,240}$' "$delete_error"; then
    tr -d '\r\n' <"$delete_error" >&2
    printf '\n' >&2
  fi
  printf 'management unavailable; configuration restore remains recoverable; plugin cleanup is pending (response body suppressed)\n' >&2
fi
CLIPROXY_CONTAINER="$container" CLIPROXY_CONFIG="$config" CLIPROXY_BACKUP="$backup" \
CLIPROXY_MANAGEMENT_KEY_FILE="$management_key_file" \
  "$repo/deploy/rollback.sh"
```

`deploy/rollback.sh` defaults to the current-host Docker layout (`CLIPROXY_CONFIG=/home/ubuntu/stacks/cliproxy/config.yaml`, `docker restart "$CLIPROXY_CONTAINER"`); `CLIPROXY_RESTART_COMMAND` overrides the restart, `CLIPROXY_CONFIG_UID`/`CLIPROXY_CONFIG_GID` declare the actual config owner when it is not root (ownership is verified, never changed), and the legacy `systemctl` path remains only when neither Docker knob is set. The usage-output cleanup targets the fixed feed path only when the usage directory exists.

The rollback is complete only after the restored configuration has been loaded by the running service container.

## Evidence and redaction

- Synthetic 401, 403, 429, threshold exhaustion, paired-sibling exclusion, healthy native round-robin, and collector redaction are executed by `acceptance_local.py`.
- Live validation stores response bodies only in mode-`0600` temporary files and checks field names; commands and comments record status codes and hashes, never key material.
- The collector rejects redirects, restricts management requests to loopback or an explicit `--allowed-origin`, caps the authenticated response at 1 MiB, requires exact schemas, RFC 3339 timestamps, integer quota ages, finite RFC JSON numbers, and one-line key files, and redacts secret-shaped string values.
- A `reconfigure_rejected` snapshot is retained only as unavailable capacity: every projected account becomes `config_error` and stale, while live installation verification fails that state.
- The collector requires an absolute output under a verified root-owned real directory, rejects symlinked or substituted parents, enforces mode `0700` on the directory and `0600` on initial and replacement files, fsyncs file data, atomically renames relative to the held directory descriptor, then fsyncs that directory.
