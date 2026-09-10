# v0.1.0 dogfood deployment

This directory records the exact non-secret inputs and checks for the first Z.ai lane deployment. It does not contain a plan key, management key, host token, or populated config.

## Preconditions proved before host changes

- Tag `v0.1.0` resolves to commit `5c758c04acbd9367d1c8fff1342bf651847dcac2`.
- The plugin-store registry source is pinned to that immutable commit. Its `registry.json` SHA-256 is `86800494fa9606971c22ee5f08872dbc3c02280ad65fe9a88fdeaf9063c5db0d`.
- `checksums.txt` pins the store archive to `6593a624135d07fdc54516a7837d873c2f54b6b4cdc0f70810c13ae0235ec2bf` and the shared library to `6a24524516054ede57a1faf91cabe91c4d200fa1a7bb54485dbacb1a30bc72c5`.
- Release workflow run `34490526081` passed the exact-image load check against `eceasy/cli-proxy-api@sha256:49a249ba0cb867d2e70ef90f23d5fa8b6e2d04bf6c73d9e666e8eee8c353b606`.
- `config.yaml.tmpl` follows `docs/ARCHITECTURE.md`: full-key pairing is rendered only on the host; `zai-coding-plan` is the sole enabled scheduler at priority `1000`.

Run the credential-free checks before opening the operator handoff:

```sh
./deploy/acceptance_local.py
```

Verify the exact registry bytes before merging the template into the host config:

```sh
registry=$(mktemp)
trap 'rm -f "$registry"' EXIT
curl --fail-with-body --fail-early --max-redirs 0 --silent --show-error \
  'https://raw.githubusercontent.com/TogetherWeOwn/cpa-plugin-zai-coding-plan/5c758c04acbd9367d1c8fff1342bf651847dcac2/registry.json' \
  >"$registry"
printf '%s  %s\n' '86800494fa9606971c22ee5f08872dbc3c02280ad65fe9a88fdeaf9063c5db0d' "$registry" | sha256sum --check --status
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
key=pathlib.Path(sys.argv[1]).read_text().strip()
if not key or "\n" in key or "\r" in key:
    raise SystemExit("management key file must contain one non-empty line")
path=pathlib.Path(sys.argv[2])
path.write_text('header = "Authorization: Bearer ' + key.replace('\\', '\\\\').replace('"', '\\"') + '"\n')
path.chmod(0o600)
PY

# Merge deploy/config.yaml.tmpl without printing the rendered credential values.
# Confirm the rendered config and containing directory are not group/world readable.
test "$(stat -c %a "$config")" = 600

# After config reload exposes the custom source, install the exact release.
curl --fail-with-body --fail-early --max-redirs 0 --silent --show-error \
  --config "$curl_config" \
  -X POST \
  -H 'Content-Type: application/json' \
  --data '{"version":"0.1.0"}' \
  'http://127.0.0.1:8317/v0/management/plugin-store/zai-coding-plan/install'

CLIPROXY_MANAGEMENT_KEY_FILE="$management_key_file" \
ZAI_CODING_PLAN_KEY_FILE="$plan_key_file" \
CLIPROXY_USAGE_DIR=/srv/cliproxy-usage \
CLIPROXY_DASHBOARD_URL=http://127.0.0.1:3000/api/telemetry/model-usage/zai \
CLIPROXY_SERVICE_UNIT=cliproxy.service \
  "$repo/deploy/verify-live.sh"
```

The plugin-store response must report `id=zai-coding-plan`, `version=0.1.0`, `install_type=github-release`, and a versioned `linux/amd64` path. The host installer verifies the release `checksums.txt`; `deploy/verify-live.sh` then proves authenticated status field names, writes sanitized `/srv/cliproxy-usage/zai.json`, and performs bounded projected-output, dashboard, and service-log scans for both management-key and plan-key markers without printing matches.

`router-capacity-source.json` is the exact Model Router capacity-source shape for one opaque Z.ai model ID. Repeat it per model ID and retain `unknownTelemetry: fail-open` during dogfood. The operator dispatcher already maps `zai/*`, `zai-openai/*`, and `glm*` to lane `zai`; the live check is a dry-run selection with the Z.ai model enabled, followed by one bounded canary issue. Do not re-pin an issue mid-run.

## Rollback

```sh
set -euo pipefail
umask 077
management_key_file=/home/ubuntu/secure-drop/cliproxy-management.key
curl_config=$(mktemp)
trap 'rm -f "$curl_config"' EXIT
python3 - "$management_key_file" "$curl_config" <<'PY'
import pathlib, sys
key=pathlib.Path(sys.argv[1]).read_text().strip()
if not key or "\n" in key or "\r" in key:
    raise SystemExit("management key file must contain one non-empty line")
path=pathlib.Path(sys.argv[2])
path.write_text('header = "Authorization: Bearer ' + key.replace('\\', '\\\\').replace('"', '\\"') + '"\n')
path.chmod(0o600)
PY
curl --fail-with-body --fail-early --max-redirs 0 --silent --show-error \
  --config "$curl_config" \
  -X DELETE \
  'http://127.0.0.1:8317/v0/management/plugins/zai-coding-plan'
install -m 0600 "$backup" "$config"
rm -f /srv/cliproxy-usage/zai.json
```

If the deployed host cannot target-unload the native library, the operator must restart the existing CLIProxy service after both install and rollback. That restart is intentionally not hidden here: it belongs on one unassigned `operator` card with the deployment-specific command and rollback.

## Evidence and redaction

- Synthetic 401, 403, 429, threshold exhaustion, paired-sibling exclusion, healthy native round-robin, and collector redaction are executed by `acceptance_local.py`.
- Live validation stores response bodies only in mode-`0600` temporary files and checks field names; commands and comments record status codes and hashes, never key material.
- The collector rejects redirects, restricts management requests to loopback or an explicit `--allowed-origin`, caps the authenticated response at 1 MiB, bounds every persisted string, and redacts secret-shaped string values.
- The collector enforces mode `0700` on the output directory, mode `0600` on initial and replacement files, fsyncs file data, atomically renames, then fsyncs the parent directory.
