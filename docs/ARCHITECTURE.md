# Architecture

## Purpose

`cpa-plugin-zai-coding-plan` is a native CLIProxyAPI (CPA) plugin that makes a pool of Z.ai GLM Coding Plan subscriptions behave as quota-aware logical accounts across CPA's Anthropic and OpenAI-compatible lanes.

Version 0.1 will:

1. discover and pair the two CPA credentials backed by each plan key;
2. estimate plan-credit consumption from CPA usage records and maintain local rolling windows;
3. keep CPA's native scheduler in control while every account is healthy, but exclude an entire paired account when it is exhausted, suspended, disabled, or invalid; and
4. expose authenticated management status and recovery operations.

The plugin does not proxy inference, rewrite requests, mint credentials, or claim its estimates are an authoritative Z.ai balance. Z.ai exposes no public plan-quota API, so local accounting is a conservative routing signal and an observed upstream `429` remains authoritative.

## Exact CLIProxyAPI plugin contract

The baseline is CLIProxyAPI v7.2.67, using the same SDK version and native-plugin pattern as the MIT-licensed reference [`hrz6976/cpa-plugin-opencode-go-pool`](https://github.com/hrz6976/cpa-plugin-opencode-go-pool):

```go
require github.com/router-for-me/CLIProxyAPI/v7 v7.2.67
```

```go
import (
    "github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
    "github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)
```

The deployed `eceasy/cli-proxy-api` v7.2.x image is a fork. Its repository was not publicly readable during this research, so a version label alone is not proof of compatibility. Release requires a load test against the exact deployed image digest.

### C ABI

On Linux, the v7.2.67 host uses `dlopen(path, RTLD_NOW | RTLD_LOCAL)`, resolves exactly `cliproxy_plugin_init` with `dlsym`, and calls it using this ABI:

```c
typedef struct {
    void* ptr;
    size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(
    void* host_ctx,
    const char* method,
    const uint8_t* request,
    size_t request_len,
    cliproxy_buffer* response
);
typedef void (*cliproxy_host_free_fn)(void* ptr, size_t len);

typedef struct {
    uint32_t abi_version;
    void* host_ctx;
    cliproxy_host_call_fn call;
    cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(
    char* method,
    uint8_t* request,
    size_t request_len,
    cliproxy_buffer* response
);
typedef void (*cliproxy_plugin_free_fn)(void* ptr, size_t len);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
    uint32_t abi_version;
    cliproxy_plugin_call_fn call;
    cliproxy_plugin_free_fn free_buffer;
    cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

typedef int (*cliproxy_plugin_init_fn)(
    const cliproxy_host_api* host,
    cliproxy_plugin_api* plugin
);
```

The `.so` exports:

```c
int cliproxy_plugin_init(
    const cliproxy_host_api* host,
    cliproxy_plugin_api* plugin
);
```

Initialization returns `0`, stores the host callback table, and fills all plugin callback fields. Both sides require `abi_version == pluginabi.ABIVersion`, which is `1` in v7.2.67. Registration uses `pluginabi.SchemaVersion`, also `1`. A buffer is freed by the side that allocated it, through that side's `free_buffer` callback.

Build with Go 1.26, `CGO_ENABLED=1`, Linux/amd64, and `-buildmode=c-shared`.

### RPC methods

Calls use a method string, JSON request bytes, and a v7.2 envelope:

```json
{"ok":true,"result":{}}
```

or:

```json
{"ok":false,"error":{"code":"plugin_error","message":"..."}}
```

Version 0.1 implements:

| Method | SDK constant | Purpose |
|---|---|---|
| `plugin.register` | `pluginabi.MethodPluginRegister` | Parse configuration, discover accounts, load state, return metadata/capabilities. |
| `plugin.reconfigure` | `pluginabi.MethodPluginReconfigure` | Atomically rebuild configuration and account indexes after CPA reload. |
| `scheduler.pick` | `pluginabi.MethodSchedulerPick` | Decline when all candidates are healthy; otherwise choose only healthy candidates. |
| `usage.handle` | `pluginabi.MethodUsageHandle` | Consume usage/failure records, update credit windows, classify 401/403/429. |
| `management.register` | `pluginabi.MethodManagementRegister` | Register management routes and optional non-sensitive resource shell. |
| `management.handle` | `pluginabi.MethodManagementHandle` | Serve status, refresh, unblock, and account configuration. |

The plugin uses `pluginabi.MethodHostLog` only for structured redacted logs. It needs no direct upstream HTTP call in v0.1 because there is no public quota endpoint.

Registration advertises schema version 1 and the `scheduler`, `usage_plugin`, and `management_api` capabilities.

## Module layout

```text
.
├── .github/
│   ├── ISSUE_TEMPLATE/
│   ├── workflows/ci.yml
│   ├── workflows/release.yml
│   └── pull_request_template.md
├── docs/ARCHITECTURE.md
├── src/
│   ├── main.go          # C ABI init/call/free/shutdown
│   ├── dispatch.go      # RPC dispatch and registration
│   ├── hostcalls.go     # envelopes and redacted host logging
│   ├── config.go        # plugin config and CPA config projection
│   ├── accounts.go      # exact-key pairing and stable auth IDs
│   ├── settings.go      # atomic 0600 persistence
│   ├── credits.go       # fixed-point credit calculation
│   ├── windows.go       # rolling five-hour and weekly ledgers
│   ├── state.go         # synchronized account health
│   ├── failures.go      # 401/403/429 classification/reset hints
│   ├── scheduler.go     # degraded-mode healthy selection
│   └── management.go    # management registration/handlers
├── config.example.yaml
├── registry.json
├── Makefile
├── go.mod
└── go.sum
```

Files may stay in `package main`, matching the reference plugin and Go's `c-shared` build constraint. Policy code must remain independently testable without CGO or wall-clock sleeps.

## Configuration

### Paired CPA credentials

One Z.ai account appears twice with the exact same key:

```yaml
claude-api-key:
  - api-key: "<same-plan-key>"
    base-url: "https://api.z.ai/api/anthropic"
    prefix: "zai"

openai-compatibility:
  - name: "zai-coding-plan"
    base-url: "https://api.z.ai/api/coding/paas/v4"
    api-key-entries:
      - api-key: "<same-plan-key>"
```

Discovery accepts only this provider name and these normalized base URLs. It pairs by full-key equality in memory, computes CPA's synthesized auth IDs using the v7.2 stable-ID algorithm, then removes the full key from derived state. An unpaired entry is a configuration error and is not managed. Never pair by suffix.

### Plugin schema

Proposed v0.1 configuration:

```yaml
plugins:
  installed:
    zai-coding-plan:
      enabled: true
      config:
        cpa-config-path: /app/config.yaml
        threshold-percent: 97
        suspend-duration: 30m
        fallback-cooldown: 10m
        state-retention: 8d
        default-plan: pro
        accounts:
          - key-suffix: "display-or-override-match"
            name: "zai-pro-1"
            plan: pro
            disabled: false
            five-hour-credits: 12000
            weekly-credits: 60000
```

| Field | Default | Rule |
|---|---:|---|
| `cpa-config-path` | `config.yaml` | Host config path readable by the plugin. |
| `threshold-percent` | `97` | Integer 1–100; either window reaching it exhausts the account. |
| `suspend-duration` | `30m` | Positive duration for 401/403. |
| `fallback-cooldown` | `10m` | Conservative 429 block when no trustworthy reset is present. |
| `state-retention` | `8d` | Must exceed one week. |
| `default-plan` | none | Optional `lite`, `pro`, or `max`; otherwise each account declares a plan. |
| `accounts[].key-suffix` | required | Override matcher only; ambiguous matches fail closed. |
| `accounts[].name` | generated | Unique display name. |
| `accounts[].plan` | inherited | `lite`, `pro`, `max`, or `custom`. |
| `accounts[].disabled` | `false` | Excludes both credentials. |
| `accounts[].five-hour-credits` | plan value | Positive custom override. |
| `accounts[].weekly-credits` | plan value | Positive custom override. |

Published plan buckets:

| Plan | Five-hour | Weekly |
|---|---:|---:|
| Lite | 2,000 | 10,000 |
| Pro | 12,000 | 60,000 |
| Max | 28,000 | 140,000 |

Reject the complete reconfiguration on ambiguous suffixes, duplicate names, invalid durations, or non-positive buckets. Keep the last valid snapshot active and expose the validation error.

## Identity and pairing

Each logical account contains:

- an internal SHA-256-derived identity, never the key itself;
- a redacted display suffix;
- the `openai-compatibility:zai-coding-plan` auth ID;
- the `claude:apikey` auth ID;
- plan buckets; and
- shared health/window state.

`byAuthID` maps both CPA credentials to the same account. A usage record or failure on either protocol updates one ledger and one health state. Key rotation creates a new identity, so the replacement key does not inherit stale blocks or usage.

The stable auth-ID implementation must reproduce host iteration order and duplicate counters. Contract tests compare it with v7.2.67 fixtures and the deployed eceasy image.

## Credit accounting

### Formula

Use exact fixed point, not binary floating point. Store integer microcredits or an equivalent scale.

GLM-5.3:

```text
(input_tokens × 6.9 + cached_input_tokens × 1.7 + output_tokens × 24) / 10,000
```

GLM-5.3-Flash:

```text
(input_tokens × 2.3 + cached_input_tokens × 0.56 + output_tokens × 8) / 10,000
```

Model names use an explicit alias allowlist derived from observed CPA records. An unknown model is `unpriced`; do not invent a rate. Expose an accounting warning and apply a documented conservative policy.

Token rules:

- cached input is charged at the cached rate;
- if CPA input includes cache reads, subtract cached input before applying the normal input rate;
- cache-write tokens not covered by the published formula use the normal input rate unless Z.ai documents otherwise;
- a failed request with no usage adds no estimated credits; and
- a failure that includes usage is accounted once.

Persist a deterministic record identity to prevent double charging after replay/reconfigure. If CPA supplies no request ID, hash stable non-secret record fields and retain a bounded dedup set.

### Rolling windows

Maintain timestamped credit events per account:

- five-hour: `(now - 5h, now]`;
- weekly: `(now - 7d, now]`.

This assumes the weekly allowance is a rolling seven-day window. Verify current Z.ai terms before v0.1.0; if it is a calendar reset, replace the boundary policy without changing the management contract.

For each window:

```text
utilization = consumed_credits / configured_bucket
resets_at   = earliest expiry after which utilization is below threshold
```

When usage is over threshold, `resets_at` is not necessarily `oldest + duration`: walk expiry events until the remaining sum becomes strictly lower than the configured threshold. Inject a clock so boundaries are deterministic.

Persist with same-directory temporary write, file `fsync`, rename, and directory `fsync` where supported.

## Health state

Shared account health values:

- `healthy` — valid, enabled, under both thresholds, no active upstream block;
- `exhausted` — local threshold reached or 429 block active;
- `suspended` — 401/403 block active;
- `disabled` — administratively disabled;
- `config_error` — invalid pair/settings.

| Signal | Result |
|---|---|
| Accounted usage below thresholds | Stay/return healthy after timers expire. |
| Either estimate reaches threshold | Exhaust until the rolling sum falls below threshold. |
| HTTP 429 | Exhaust; parse bounded reset hints or use fallback cooldown. |
| HTTP 401/403 | Suspend for configured duration. |
| Window/timer expiry | Recompute and clear automatically. |
| Management unblock | Clear transient flags and recompute retained usage; never erase consumption. |
| Key rotation | Preserve state only for the same hashed identity. |

A real 429 overrides a lower estimate. Repeated failures extend, never shorten, an active block. Treat reset hints as untrusted: bound body/header lengths, reject malformed/past/unreasonably distant values, and expose the chosen reason/reset.

## Scheduler

For `scheduler.pick`:

1. Map candidates to logical accounts.
2. Recompute expired health.
3. If no managed account is impaired, return `Handled:false`; CPA retains native round-robin/session affinity.
4. In degraded state, remove every candidate belonging to an impaired account, including its sibling credential.
5. Round-robin among healthy candidates per provider/model, preserving header-derived stickiness when possible (`X-Session-ID`, `Session-Id`, `Session_id`, `X-Client-Request-Id`).
6. If no managed healthy candidate remains, return `Handled:false` so CPA produces its normal retry/error behavior rather than selecting a known-bad credential.

The scheduler performs no disk, network, or host callback while holding its lock.

## Management API

`management.register` adds routes mounted by CPA under `/v0/management` and protected by CPA's management key:

| Method/path | Purpose |
|---|---|
| `GET /v0/management/plugins/zai-coding-plan/status` | Redacted account windows and health. |
| `POST /v0/management/plugins/zai-coding-plan/refresh` | Reload/compact ledger, expire windows, recompute state. |
| `POST /v0/management/plugins/zai-coding-plan/unblock` | Clear transient blocks, then recompute retained usage. |
| `POST /v0/management/plugins/zai-coding-plan/account-config` | Save/clear non-key plan metadata. |

Collector-facing status fields are exact:

```json
{
  "version": "0.1.0",
  "generated_at": "2026-09-08T05:00:00Z",
  "accounts": [
    {
      "name": "zai-pro-1",
      "key_suffix": "redacted",
      "plan": "pro",
      "five_hour_utilization": 0.42,
      "weekly_utilization": 0.18,
      "five_hour_resets_at": "2026-09-08T08:42:00Z",
      "weekly_resets_at": "2026-09-14T03:21:00Z",
      "health": "healthy"
    }
  ]
}
```

Treat utilizations as ratios unless the `cliproxy_usage_snapshot.py` contract fixture proves percentages. Additional fields may expose consumed/bucket credits, block reason, timestamps, and warnings, but never full keys, key hashes, request bodies, authorization headers, or management credentials.

CPA must reject unauthenticated management HTTP requests before dispatch. If a resource route provides UI, it serves only a static shell; data still comes from the authenticated management endpoint.

## Persistence and concurrency

```text
<auth-dir>/zai-coding-plan/
├── settings.json
└── ledger.json
```

Requirements:

- directory mode `0700`;
- files mode `0600`, including replacements;
- reject symlink targets and use atomic same-directory replacement;
- never persist the API key;
- key settings by hashed account identity so renames survive but rotations reset;
- one in-process mutex protects the active snapshot, state, dedup set, and cursors;
- copy a persistence snapshot under lock, then write after releasing it; and
- shutdown flush is idempotent.

Quarantine corrupt state with a redacted diagnostic. Start conservatively with an accounting warning rather than silently replacing it.

## Data flow

```text
CPA config YAML
    │ plugin.register / plugin.reconfigure
    ▼
validate ── discover exact key pairs ── load 0600 ledger
    │
    ├── both auth IDs ─────────────────────────────┐
    │                                              │
CPA completed request                              │ CPA candidates
    │ usage.handle                                 │ scheduler.pick
    ▼                                              ▼
normalize/model ── fixed-point credits             recompute health
    │                   │                           │
    ├── 401/403/429     │                           ├── all healthy → host handles
    ▼                   ▼                           └── degraded → healthy candidate
shared health ◄── rolling 5h/7d ledger
    │
    ├── atomic persistence
    └── authenticated status → collector lane `zai`
```

## Threat model

### Assets

Z.ai keys, CPA management authentication, usage/account state, local ledger integrity, and the native `.so` executing in CPA.

| Threat | Control |
|---|---|
| Keys leak through logs/status/errors | Keys exist only transiently during discovery. Central redaction and tests scan serialized outputs/logs for fixture keys. |
| Wrong pairing through suffix collision | Pair only by full-key equality. Suffix matching is override-only and ambiguity fails closed. |
| Unauthenticated quota/account disclosure | Data only on CPA management-key routes. Integration-test unauthorized/authorized HTTP behavior. Resource shell has no data. |
| Local disclosure | `0700` directory, `0600` files, unprivileged CPA user, no secrets in filenames. |
| Ledger tampering creates false capacity | Validate schema, non-negative bounded values, model/rate IDs, timestamps, sizes. Corruption causes conservative degraded state. Unblock never deletes usage. |
| ABI mismatch crashes CPA | Pin v7.2.67 SDK, inspect exported symbol, then load-test exact eceasy image digest. |
| Malicious reset hints deny service | Allowlist fields, cap lengths/future duration, reject malformed/past values. |
| Usage replay double charges | Persist bounded deterministic dedup IDs; test replay after restart. |
| Concurrency race/deadlock | No host callback or I/O under state mutex; run `go test -race`. |
| Release substitution | Protected main, PRs, pinned modules, CI, tagged checksums, store checksum verification. |

Key discovery/persistence and management routing require independent security review at the exact commit SHA. CI builds release artifacts from reviewed semver tags; do not publish locally built binaries.

## CI and release gates

Pull requests must pass:

```text
gofmt check
go vet ./...
golangci-lint run
go test -race ./...
CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -buildmode=c-shared ./src
nm -D <artifact> | grep ' cliproxy_plugin_init$'
```

A release job loads the `.so` into the exact `eceasy/cli-proxy-api` v7.2.x image digest, confirms registration and all advertised capabilities, and calls authenticated status. This is mandatory before dogfood.

A `vMAJOR.MINOR.PATCH` tag produces a versioned `.so`, CPA store zip, SHA-256 checksums, changelog release notes, and matching `registry.json`. Every PR receives exact-SHA review; keys/management changes also receive security review.

## Test strategy

Unit/property coverage:

- exact formula vectors, large counts, zero usage, and rounding boundaries;
- cache semantics and unknown models;
- rolling boundary and earliest threshold reset with multiple events;
- Lite/Pro/Max/custom bucket validation;
- exact pairing, duplicates, missing siblings, ambiguous suffixes, rotation;
- reset headers/body variants and adversarial values;
- cross-protocol 401/403/429 health;
- healthy scheduler passthrough and degraded exclusion/round-robin/stickiness;
- dedup across replay/restart;
- permissions, atomic replacement, corrupt files, symlinks; and
- collector status golden JSON.

ABI/integration coverage:

- Go 1.26 CGO `.so` build and exported symbol;
- upstream v7.2.67 load baseline;
- exact deployed eceasy image load;
- paired-account synthetic usage/failure flow;
- management-key enforcement; and
- persistence across shutdown/reload without double count.

## Operational and ToS note

The Z.ai GLM Coding Plan is licensed for supported coding tools, and Claude Code is one of those tools. Our agent workloads are Claude Code sessions. The plugin therefore supports plan use for that supported coding client; it must not turn the subscription into an unrestricted public inference/resale service. Re-check current Z.ai terms before any release that materially changes clients or traffic patterns.

The repository and reference plugin are MIT-licensed. Preserve required notices when adapting reference code.

## Open verification items before v0.1.0

1. Record the immutable digest/version of the deployed `eceasy/cli-proxy-api` image and pass the native load test. The private fork could not be read directly in this run.
2. Confirm whether Z.ai's weekly bucket is rolling seven days or a calendar boundary.
3. Capture redacted real `pluginapi.UsageRecord` fixtures to settle cache-token semantics and model aliases.
4. Confirm whether `cliproxy_usage_snapshot.py` expects utilization ratios or percentages and lock it with a golden test.
5. Capture real redacted Z.ai 429/reset variants.
