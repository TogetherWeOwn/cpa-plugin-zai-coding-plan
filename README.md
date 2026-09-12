# Z.AI Coding Plan for CLIProxyAPI

`zai-coding-plan` is a native Linux/amd64 plugin for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI). It pairs the Anthropic and OpenAI-compatible credentials backed by each Z.AI Coding Plan key, tracks the plan's five-hour and weekly quota, keeps impaired accounts out of scheduling, and exposes redacted status through CLIProxyAPI's authenticated management API.

> [!IMPORTANT]
> Version `0.2.0` is a release candidate until the exact release commit passes CI, immutable-image compatibility, code-review, and security-review gates and the `v0.2.0` tag is published. Do not deploy an untagged artifact as a release.

## Capabilities

The plugin advertises these CLIProxyAPI capabilities:

- `scheduler`: explicitly delegate healthy traffic to native round-robin, select only healthy candidates while the pool is degraded, and fail closed when all managed capacity is impaired;
- `usage_plugin`: consume usage and failure records to maintain shared health and fallback accounting across both credential protocols;
- `management_api`: expose authenticated status, refresh, unblock, and non-secret account-configuration operations.

Quota data comes from Z.AI's plan endpoint when available. The token-based credit formula is a conservative fallback when that endpoint cannot be read. Status reports whether quota is authoritative or estimated and includes freshness and off-peak fields.

## Requirements

- Linux amd64
- CLIProxyAPI v7.2.x with native plugin ABI/schema version 1
- exactly one enabled scheduler plugin, with `zai-coding-plan` configured at priority `1000`
- a Z.AI Coding Plan used through supported coding tools
- Go 1.26 and a GCC-compatible C toolchain only when building from source

The supported host range is CLIProxyAPI v7.2.67 through the newest `eceasy/cli-proxy-api` v7.2.x tag that passes the compatibility matrix. CI always tests the historical v7.2.67 baseline, the operator-recorded deployed image, and the latest published tag. See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) for the pinned SDK, ABI, scheduler-exclusivity, persistence, and threat-model contracts.

## Install

Release assets use one version consistently:

```text
zai-coding-plan-v0.2.0.so
zai-coding-plan_0.2.0_linux_amd64.zip
checksums.txt
```

### Install the shared library

Download the versioned shared library and `checksums.txt` from the same GitHub release, then verify before installation:

```sh
version=0.2.0
sha256sum --check checksums.txt
install -m 0755 "zai-coding-plan-v${version}.so" /path/to/cliproxy/plugins/linux/amd64/
```

### Install the plugin-store archive

Verify `checksums.txt`, then supply `zai-coding-plan_0.2.0_linux_amd64.zip` to the CLIProxyAPI plugin-store flow. The archive contains exactly one root-level entry named `zai-coding-plan.so`.

Restart CLIProxyAPI after changing the native plugin. Confirm registration and all three capabilities before sending traffic. The release is not valid until CI passes the immutable host matrix: the deployed digest from [`deploy/deployed-host-image.json`](deploy/deployed-host-image.json), the latest `eceasy/cli-proxy-api` tag resolved to its linux/amd64 manifest at run time, and the historical baseline from [`.github/host-images.json`](.github/host-images.json).

### Host compatibility matrix

The matrix is maintained in three places:

- the operator updates [`deploy/deployed-host-image.json`](deploy/deployed-host-image.json) immediately after a host deployment, recording the exact linux/amd64 manifest digest;
- [`.github/host-images.json`](.github/host-images.json) keeps the v7.2.67 baseline while that version remains supported;
- [`.github/scripts/resolve-host-images.sh`](.github/scripts/resolve-host-images.sh) queries Docker Hub on every run and resolves the highest semantic v7.2.x tag to an immutable linux/amd64 manifest digest.

For every image, CI loads the plugin, checks registration and all three advertised capabilities, sends real requests through the host scheduler, and requires HTTP `200` round trips for `zai/smoke-model` and `zai-openai/smoke-model` against an isolated TLS stub. Any `zai_unmanaged_candidate`, scheduler rejection, missing upstream request, or non-200 response fails the job. [`.github/workflows/host-compatibility.yml`](.github/workflows/host-compatibility.yml) runs this check on pull requests, `main`, manual dispatch, and daily at 07:17 UTC. A failed scheduled run creates a GitHub Actions notification; branch protection should require **Host compatibility / Deployed, latest, and baseline host images** before merge.

## Configure CLIProxyAPI credentials

A single plan key must appear in both provider sections. Pairing uses full-key equality in memory; a suffix is only a redacted display and override selector.

```yaml
claude-api-key:
  - api-key: ${ZAI_CODING_PLAN_KEY}
    base-url: https://api.z.ai/api/anthropic
    prefix: zai

openai-compatibility:
  - name: zai-coding-plan
    base-url: https://api.z.ai/api/coding/paas/v4
    api-key-entries:
      - api-key: ${ZAI_CODING_PLAN_KEY}
```

The environment placeholder is illustrative; use CLIProxyAPI's supported secret/config mechanism. Never commit a populated configuration file.

## Configure the plugin

Start from [`config.example.yaml`](config.example.yaml). Plugin settings belong directly under `plugins.configs.zai-coding-plan`; `enabled` and `priority` are host fields at the same level.

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    zai-coding-plan:
      enabled: true
      priority: 1000
      cpa-config-path: /app/config.yaml
      quota-refresh-interval: 2m
      authoritative-max-age: 5m
      threshold-percent: 97
      suspend-duration: 30m
      fallback-cooldown: 10m
      state-retention: 8d
      default-plan: pro
      accounts:
        - key-suffix: replace-with-unique-suffix
          name: zai-pro-1
          plan: pro
          disabled: false
```

| Setting | Default | Meaning |
|---|---:|---|
| `plugins.enabled` | `false` | Global native-plugin switch; set it to `true`. |
| `plugins.dir` | `plugins` | Native plugin root; Linux/amd64 libraries are loaded from `<dir>/linux/amd64/`. |
| `enabled` | host default | Per-plugin enable switch under `plugins.configs.zai-coding-plan`. |
| `priority` | `1000` required | Host-level scheduler priority. V0.1 also rejects any second enabled scheduler plugin. |
| `cpa-config-path` | `config.yaml` | CLIProxyAPI configuration used to discover exact credential pairs and `auth-dir`. |
| `quota-refresh-interval` | `2m` | Base poll interval; jittered polling accepts only `1m`–`3m`. |
| `authoritative-max-age` | `5m` | Maximum age before authoritative data becomes stale; must exceed maximum polling jitter. |
| `threshold-percent` | `97` | Either quota bucket reaching this percentage exhausts the account. |
| `suspend-duration` | `30m` | Conservative block after `401` or `403`. |
| `fallback-cooldown` | `10m` | Block after `429` when no trustworthy reset time is available. |
| `state-retention` | `8d` | Local estimator and deduplication retention; must exceed one week. |
| `default-plan` | none | Optional `lite`, `pro`, or `max` fallback plan. |
| `accounts` | `[]` | Required suffix selectors with optional names, plan overrides, custom buckets, and administrative disable state. |

A custom plan sets `plan: custom`, `five-hour-credits`, and `weekly-credits`. Suffixes must identify one full-key pair unambiguously; names must be unique.

Local state and non-secret settings are stored atomically under `<auth-dir>/zai-coding-plan/`. The directory is mode `0700`, files are mode `0600`, and provider keys are never persisted.

## Management API

CLIProxyAPI mounts plugin routes under `/v0/management` and authenticates them with its management key. The plugin does not implement a second authentication scheme.

```sh
management_url=http://127.0.0.1:8317
curl --fail-with-body \
  -H "Authorization: Bearer ${CLIPROXY_MANAGEMENT_KEY}" \
  "${management_url}/v0/management/plugins/zai-coding-plan/status"
```

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/v0/management/plugins/zai-coding-plan/status` | Redacted per-account quota, reset times, source, freshness, off-peak state, health, and integrity warnings. |
| `POST` | `/v0/management/plugins/zai-coding-plan/refresh` | Force a bounded quota refresh for configured accounts. Body must be empty or `{}`. |
| `POST` | `/v0/management/plugins/zai-coding-plan/unblock` | Clear transient blocks without erasing retained quota or usage. Optional JSON: `{"account":"name-or-suffix"}`. |
| `POST` | `/v0/management/plugins/zai-coding-plan/account-config` | Save or clear validated non-secret account and polling settings. |

The plugin also registers a **Z.ai Quota** resource entry at `/v0/resource/plugins/zai-coding-plan/status`, so current Management Center builds place Z.ai in the left navigation. CLIProxyAPI resource routes are intentionally unauthenticated; the resource page therefore contains no quota payload and no management credential. Detailed quota remains on the authenticated status endpoint above until the Management Center accepts native plugin-backed Quota-page cards.

Status accounts include `five_hour_utilization`, `weekly_utilization`, `five_hour_resets_at`, `weekly_resets_at`, `quota_source`, `quota_observed_at`, `quota_age_seconds`, `quota_stale`, `offpeak`, `health`, and bounded integrity-warning fields. They never expose keys or key hashes.

`account-config` requires an `account` name or suffix. It accepts non-secret fields such as `name`, `plan`, `disabled`, `five_hour_credits`, `weekly_credits`, `threshold_percent`, `polling_interval`, `authoritative_max_age`, and `timeout`; `{"account":"...","clear":true}` restores the base configuration for that account.

## Build and verify

```sh
make fmt-check
make vet
make test
make lint
make test-release
make validate-source
make package VERSION=0.2.0
make validate-release VERSION=0.2.0 TAG=v0.2.0
(cd dist && sha256sum --check checksums.txt)
```

`make package` builds a versioned `.so`, creates the plugin-store zip, writes SHA-256 checksums, and fails if an expected artifact is empty. CI additionally checks the exported `cliproxy_plugin_init` symbol, archive contents, source secret scan, and license/notice requirements.

## Release policy

A v0.2.0 tag is created only after all implementation slices are merged and the exact release commit has:

1. green formatting, vet, race-test, lint, build, packaging, secret-scan, and license/notice checks;
2. machine-checked tag, binary, archive, registry, changelog, and checksum consistency;
3. the immutable host compatibility matrix green for both release-required rows (the operator-reported deployed digest and the latest Docker Hub tag), with the supported v7.2.67 baseline also exercised; each row must advertise scheduler, usage, and management capabilities, return management `401`/redacted `200`, and serve both prefixed inference paths through a real scheduler pick;
4. an exact-SHA code review; and
5. a separate exact-SHA security review for credential handling and management operations.

The release tag must point to that reviewed commit after it is merged to `main`. The tag workflow validates strict semantic-version syntax and proves the event commit is reachable from `origin/main` before any repository build or packaging command receives the version. Tags trigger the release workflow. Do not publish locally built artifacts or move an existing tag.

## Security and scope

Never commit, print, or return plan keys, authorization headers, management credentials, raw request bodies, or key hashes. Report vulnerabilities privately as described in [`SECURITY.md`](SECURITY.md).

Z.AI Coding Plan access is for supported coding tools; Claude Code is a supported tool and this project's intended agent workloads are Claude Code sessions. This plugin must not turn the subscription into a public inference or resale service.

## Contributing

See [`CONTRIBUTING.md`](CONTRIBUTING.md), [`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md), [`CHANGELOG.md`](CHANGELOG.md), [`SECURITY.md`](SECURITY.md), [`NOTICE`](NOTICE), and [`LICENSE`](LICENSE).
