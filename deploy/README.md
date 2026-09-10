# v0.1.0 dogfood deployment

This directory records the exact non-secret inputs and checks for the first Z.ai lane deployment. It does not contain a plan key, management key, host token, or populated config.

## Preconditions proved before host changes

- Tag `v0.1.0` resolves to commit `5c758c04acbd9367d1c8fff1342bf651847dcac2`.
- `checksums.txt` pins the store archive to `6593a624135d07fdc54516a7837d873c2f54b6b4cdc0f70810c13ae0235ec2bf` and the shared library to `6a24524516054ede57a1faf91cabe91c4d200fa1a7bb54485dbacb1a30bc72c5`.
- Release workflow run `34490526081` passed the exact-image load check against `eceasy/cli-proxy-api@sha256:49a249ba0cb867d2e70ef90f23d5fa8b6e2d04bf6c73d9e666e8eee8c353b606`.
- `config.yaml.tmpl` follows `docs/ARCHITECTURE.md`: full-key pairing is rendered only on the host; `zai-coding-plan` is the sole enabled scheduler at priority `1000`.

Run the credential-free checks before opening the operator handoff:

```sh
./deploy/acceptance_local.py
```

## Exact host install and validation

The operator card must substitute the deployment's real container/config paths, but these commands are the invariant core. The management key and Z.ai key are read from root-only files; neither is placed in an argument, output, or board comment.

```sh
set -euo pipefail
repo=/home/ubuntu/cpa-plugin-zai-coding-plan
config=/home/ubuntu/cliproxy/config.yaml
backup="${config}.pre-zai-$(date -u +%Y%m%dT%H%M%SZ)"
cp -a "$config" "$backup"

# Merge deploy/config.yaml.tmpl without printing the rendered credential values.
# Confirm the rendered config and containing directory are not group/world readable.
test "$(stat -c %a "$config")" = 600

# After config reload exposes the custom source, install the exact release.
management_key=$(</home/ubuntu/secure-drop/cliproxy-management.key)
curl --fail-with-body --silent --show-error \
  -X POST \
  -H "Authorization: Bearer ${management_key}" \
  -H 'Content-Type: application/json' \
  --data '{"version":"0.1.0"}' \
  'http://127.0.0.1:8317/v0/management/plugin-store/zai-coding-plan/install'

CLIPROXY_MANAGEMENT_KEY_FILE=/home/ubuntu/secure-drop/cliproxy-management.key \
CLIPROXY_USAGE_DIR=/srv/cliproxy-usage \
  "$repo/deploy/verify-live.sh"
```

The plugin-store response must report `id=zai-coding-plan`, `version=0.1.0`, `install_type=github-release`, and a versioned `linux/amd64` path. The host installer verifies the release `checksums.txt`; `deploy/verify-live.sh` then proves authenticated status field names and writes sanitized `/srv/cliproxy-usage/zai.json`.

`router-capacity-source.json` is the exact Model Router capacity-source shape for one opaque Z.ai model ID. Repeat it per model ID and retain `unknownTelemetry: fail-open` during dogfood. The operator dispatcher already maps `zai/*`, `zai-openai/*`, and `glm*` to lane `zai`; the live check is a dry-run selection with the Z.ai model enabled, followed by one bounded canary issue. Do not re-pin an issue mid-run.

## Rollback

```sh
set -euo pipefail
management_key=$(</home/ubuntu/secure-drop/cliproxy-management.key)
curl --fail-with-body --silent --show-error \
  -X DELETE \
  -H "Authorization: Bearer ${management_key}" \
  'http://127.0.0.1:8317/v0/management/plugins/zai-coding-plan'
install -m 0600 "$backup" "$config"
rm -f /srv/cliproxy-usage/zai.json
```

If the deployed host cannot target-unload the native library, the operator must restart the existing CLIProxy service after both install and rollback. That restart is intentionally not hidden here: it belongs on one unassigned `operator` card with the deployment-specific command and rollback.

## Evidence and redaction

- Synthetic 401, 403, 429, threshold exhaustion, paired-sibling exclusion, healthy native round-robin, and collector redaction are executed by `acceptance_local.py`.
- Live validation stores response bodies only in secure temporary files and checks field names; commands and comments record status codes and hashes, never key material.
- The collector refuses secret-like JSON field names, requires `key_suffix: redacted`, caps the authenticated response at 1 MiB, and atomically replaces `zai.json`.
