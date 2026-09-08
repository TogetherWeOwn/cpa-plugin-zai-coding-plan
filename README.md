# Z.AI Coding Plan for CLIProxyAPI

`zai-coding-plan` is a native Linux/amd64 plugin for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI). It pairs the Anthropic and OpenAI-compatible credentials backed by each Z.AI Coding Plan key, tracks the plan's five-hour and weekly quota, keeps exhausted accounts out of scheduling, and exposes redacted status through CLIProxyAPI's authenticated management API.

> [!IMPORTANT]
> Version `0.1.0` is not released yet. The current branch contains the plugin scaffold and release contract while the account, quota, scheduler, and management implementation lands. Do not advertise or deploy an unreleased artifact as production-ready. The release gate includes loading the final `.so` in the exact approved `eceasy/cli-proxy-api` v7.2.x image digest.

## Capabilities

The completed v0.1.0 plugin will advertise these CLIProxyAPI plugin capabilities:

- `scheduler`: preserve native scheduling while healthy and exclude both credentials for an impaired logical account;
- `usage_plugin`: consume usage/failure records and maintain shared account health;
- `management_api`: expose authenticated status, refresh, unblock, and account-configuration operations.

Quota data comes from Z.AI's plan endpoint when available. The token-based credit formula is a conservative fallback when that endpoint cannot be read. Status reports whether quota is authoritative or estimated and includes the current off-peak flag.

## Requirements

- Linux amd64
- Go 1.26 and a GCC-compatible C toolchain when building from source
- CLIProxyAPI v7.2.x with native plugin ABI/schema version 1
- a Z.AI Coding Plan used through supported coding tools

See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) for the pinned SDK and ABI contract.

## Install

Release assets use one version consistently:

```text
zai-coding-plan-v0.1.0.so
zai-coding-plan_0.1.0_linux_amd64.zip
checksums.txt
```

### Install the shared library

```sh
version=0.1.0
sha256sum --check checksums.txt
install -m 0755 "zai-coding-plan-v${version}.so" /path/to/cliproxy/plugins/linux/amd64/
```

### Install the plugin-store archive

Verify `checksums.txt`, then supply `zai-coding-plan_0.1.0_linux_amd64.zip` to the CLIProxyAPI plugin-store flow. The archive contains one root-level entry named `zai-coding-plan.so`.

Restart CLIProxyAPI after changing the native plugin. Confirm the startup log reports plugin registration and all three capabilities before sending traffic. The release is not valid until CI has performed that check against the exact approved image digest.

## Configure CLIProxyAPI credentials

A single plan key must appear in both provider sections. Pairing uses full-key equality in memory; a suffix is only a redacted display/override selector.

```yaml
claude-api-key:
  - api-key: "${ZAI_CODING_PLAN_KEY}"
    base-url: "https://api.z.ai/api/anthropic"
    prefix: "zai"

openai-compatibility:
  - name: "zai-coding-plan"
    base-url: "https://api.z.ai/api/coding/paas/v4"
    api-key-entries:
      - api-key: "${ZAI_CODING_PLAN_KEY}"
```

The literal environment placeholder above is illustrative; use CLIProxyAPI's supported secret/config mechanism. Never commit a populated configuration file.

## Configure the plugin

Start from [`config.example.yaml`](config.example.yaml). The intended v0.1 schema is:

```yaml
plugins:
  installed:
    zai-coding-plan:
      enabled: true
      config:
        cpa-config-path: /app/config.yaml
        quota-endpoint: https://api.z.ai/api/monitor/usage/quota/limit
        quota-refresh-interval: 2m
        threshold-percent: 97
        suspend-duration: 30m
        fallback-cooldown: 10m
        state-retention: 8d
        default-plan: pro
        accounts:
          - key-suffix: "abcd"
            name: "zai-pro-1"
            plan: pro
            disabled: false
```

| Setting | Default | Meaning |
|---|---:|---|
| `cpa-config-path` | `config.yaml` | CLIProxyAPI configuration used to discover exact credential pairs. |
| `quota-endpoint` | Z.AI monitor URL | HTTPS endpoint queried with the corresponding plan key. |
| `quota-refresh-interval` | `2m` | Base poll interval; implementations jitter polls and accept only `1m`–`3m`. |
| `threshold-percent` | `97` | Either quota bucket reaching this percentage exhausts the account. |
| `suspend-duration` | `30m` | Conservative block after `401` or `403`. |
| `fallback-cooldown` | `10m` | Block after `429` when no trustworthy reset time is available. |
| `state-retention` | `8d` | Local estimator/dedup retention; must exceed one week. |
| `default-plan` | none | Optional `lite`, `pro`, or `max` fallback plan. |
| `accounts` | `[]` | Redacted display names, plan overrides, and administrative disable state. |

Plugin state lives under `<auth-dir>/zai-coding-plan/`, with directory mode `0700` and file mode `0600`. It must never persist the plan key.

## Management API

CLIProxyAPI mounts plugin routes under `/v0/management` and authenticates them with its management key. The plugin never implements a second authentication scheme.

```sh
management_url=http://127.0.0.1:8317
curl --fail-with-body \
  -H "Authorization: Bearer ${CLIPROXY_MANAGEMENT_KEY}" \
  "${management_url}/v0/management/plugins/zai-coding-plan/status"
```

Planned v0.1.0 routes:

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/v0/management/plugins/zai-coding-plan/status` | Redacted per-account quota, reset times, source, off-peak state, and health. |
| `POST` | `/v0/management/plugins/zai-coding-plan/refresh` | Poll quota now, compact fallback state, and recompute health. |
| `POST` | `/v0/management/plugins/zai-coding-plan/unblock` | Clear transient blocks and recompute without manufacturing capacity. |
| `POST` | `/v0/management/plugins/zai-coding-plan/account-config` | Save or clear non-secret plan metadata. |

A status account includes the collector contract fields `five_hour_utilization`, `weekly_utilization`, `five_hour_resets_at`, `weekly_resets_at`, and `health`. Final examples will be copied from golden integration fixtures rather than hand-written before implementation.

## Build and verify

```sh
make fmt-check
make vet
make test
make lint
make package VERSION=0.1.0
(cd dist && sha256sum --check checksums.txt)
```

`make package` builds a versioned `.so`, creates the plugin-store zip, writes SHA-256 checksums, and fails if an expected artifact is empty. CI additionally checks the exported `cliproxy_plugin_init` symbol and archive contents.

## Release policy

A v0.1.0 tag is created only after all implementation slices are merged and the exact release commit has:

1. green formatting, vet, race-test, lint, build, packaging, secret-scan, and license/notice checks;
2. machine-checked tag, binary, archive, registry, changelog, and checksum consistency;
3. a load test in the immutable approved `eceasy/cli-proxy-api` v7.2.x image that observes scheduler, usage, and management capabilities and an authenticated status response;
4. an exact-SHA code review; and
5. a separate exact-SHA security review for credential handling and management operations.

Tags trigger the release workflow. Do not publish locally built artifacts or move an existing tag.

## Security and scope

Never commit, print, or return plan keys, authorization headers, management credentials, raw request bodies, or key hashes. Report vulnerabilities privately as described in [`SECURITY.md`](SECURITY.md).

Z.AI Coding Plan access is for supported coding tools; Claude Code is a supported tool and this project's intended agent workloads are Claude Code sessions. This plugin must not turn the subscription into a public inference or resale service.

## Contributing

See [`CONTRIBUTING.md`](CONTRIBUTING.md), [`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md), [`CHANGELOG.md`](CHANGELOG.md), and [`LICENSE`](LICENSE).
