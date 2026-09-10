# Architecture

## Purpose

`cpa-plugin-zai-coding-plan` is a native CLIProxyAPI (CPA) plugin that makes a pool of Z.ai GLM Coding Plan subscriptions behave as quota-aware logical accounts across CPA's Anthropic and OpenAI-compatible lanes.

Version 0.1:

1. discover and pair the two CPA credentials backed by each plan key;
2. poll Z.AI's plan-quota endpoint for authoritative five-hour and weekly utilization, with local credit estimation as a degraded fallback;
3. remain CPA's sole active scheduler plugin, delegate to CPA's native scheduler while every account is healthy, and exclude an entire paired account when it is exhausted, suspended, disabled, or invalid; and
4. expose authenticated management status and recovery operations.

The plugin does not proxy inference, rewrite requests, or mint credentials. Its primary quota signal is `GET https://api.z.ai/api/monitor/usage/quota/limit`, queried per account with that account's plan key. Local accounting is a conservative fallback when the endpoint fails, and an observed upstream `429` remains authoritative.

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

The deployed `eceasy/cli-proxy-api` v7.2.x image is a fork. Its repository was not publicly readable during this research, so a version label alone is not proof of compatibility. Release requires a load test against the exact approved image digest; the immutable linux/amd64 manifest digest is recorded in `.github/release-host-image.json` rather than replaced by a mutable tag.

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
    const char* method,
    const uint8_t* request,
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
| `scheduler.pick` | `pluginabi.MethodSchedulerPick` | Explicitly delegate healthy traffic to built-in round-robin, select healthy candidates during degradation, and hard-fail when managed capacity is exhausted. |
| `usage.handle` | `pluginabi.MethodUsageHandle` | Consume usage/failure records, update credit windows, classify 401/403/429. |
| `management.register` | `pluginabi.MethodManagementRegister` | Register management routes and optional non-sensitive resource shell. |
| `management.handle` | `pluginabi.MethodManagementHandle` | Serve status, refresh, unblock, and account configuration. |

The plugin uses `pluginabi.MethodHostLog` only for structured redacted logs. A background quota client performs bounded HTTPS GETs to Z.AI's documented monitor endpoint outside scheduler critical sections. It authenticates with the paired plan key, redacts all request/response diagnostics, applies timeouts and response-size limits, and jitter-polls each account every one to three minutes.

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
│   ├── quota.go         # bounded authoritative quota polling/parsing
│   ├── credits.go       # fixed-point fallback credit calculation
│   ├── windows.go       # fallback five-hour and weekly ledgers
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

Discovery accepts only this provider name and these normalized base URLs. It pairs by full-key equality in memory and computes CPA's synthesized auth IDs using the v7.2 stable-ID algorithm. The full key remains only in the in-memory account object for quota authentication; it is never persisted, returned, or logged. Never pair by suffix.

A valid logical account has exactly one matching Claude entry and exactly one matching OpenAI-compatible key entry. A recognized Z.ai entry that is unpaired, duplicated on either side, or ambiguously matched makes the whole candidate snapshot invalid. Registration/reconfiguration rejects that snapshot and keeps the last valid snapshot; first registration fails rather than leaving recognized Z.ai credentials unmanaged and eligible for built-in selection.

### Plugin schema

Implemented v0.1 configuration:

```yaml
plugins:
  enabled: true
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
        - key-suffix: "display-or-override-match"
          name: "zai-pro-1"
          plan: pro
          disabled: false
          five-hour-credits: 12000
          weekly-credits: 60000
```

| Field | Default | Rule |
|---|---:|---|
| `priority` | `1000` | Host-level `PluginInstanceConfig` field, not plugin payload. V0.1 requires this plugin to be the only enabled scheduler; the high value makes accidental lower-priority schedulers non-winning but is not a substitute for exclusivity validation. |
| `cpa-config-path` | `config.yaml` | Host config path readable by the plugin. |
| `quota-refresh-interval` | `2m` | Base jittered interval; accept only one to three minutes in v0.1. |
| `authoritative-max-age` | `5m` | Maximum age for scheduling from the last successful quota response before switching to the estimator. Must exceed the maximum jittered poll interval. |
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

Reject the complete initial registration on invalid account cardinality, ambiguous suffixes, duplicate names, invalid durations, or non-positive buckets. An invalid reconfiguration retains the last valid internal snapshot and records a bounded redacted validation error, but returns an `invalid_config` RPC envelope; CPA v7.2.67 consequently removes the plugin from the rebuilt capability snapshot rather than serving a falsely valid registration. The exact-image integration test verifies that valid configuration registers and that invalid live configuration withdraws scheduler capability. An invalid initial registration does not advertise scheduler capability.

## Identity and pairing

Each logical account contains:

- an internal HMAC-SHA256-derived identity, never the key itself;
- a redacted display suffix;
- the `openai-compatibility:zai-coding-plan` auth ID;
- the `claude:apikey` auth ID;
- plan buckets; and
- shared health/window state.

`byAuthID` maps both CPA credentials to the same account. A usage record or failure on either protocol updates one ledger and one health state. Key rotation creates a new identity, so the replacement key does not inherit stale blocks or usage.

The stable auth-ID implementation must reproduce host iteration order and duplicate counters. Contract tests compare it with v7.2.67 fixtures and the deployed eceasy image.

## Quota and fallback credit accounting

### Authoritative quota

For each paired account, poll:

```text
GET https://api.z.ai/api/monitor/usage/quota/limit
Authorization: Bearer <plan key>
```

A successful response contains `CREDIT_LIMIT` records. Map `unit: 3, number: 5` to the five-hour bucket and `unit: 6, number: 1` to the weekly bucket. Treat `currentValue / usage` as utilization, `remaining` as advisory cross-check data, and `nextResetTime` as an epoch-millisecond reset instant. Require exactly one valid record for each bucket; missing or duplicate records make the response unusable. The server-reported plan `level` selects the normal Lite/Pro/Max defaults unless an explicit, validated account override is required.

Quota state records the source (`quota_api` or `estimate`), last successful refresh, next reset times, and any redacted warning. Poll asynchronously every one to three minutes with deterministic jitter per account; coalesce concurrent refreshes, bound connect/total timeouts and response bytes, validate all numeric fields, and retain the last result only through `authoritative-max-age`. Scheduler calls perform no network I/O.

When a poll recovers after estimated mode, replace routing utilization and resets atomically with the authoritative response. Retain the estimator ledger only for future outages; do not add estimated consumption to `currentValue`, and do not let a lower estimate clear an upstream 429 block. Record the source transition and divergence as redacted diagnostics.

The quota client keeps each plan key only in process memory for the active configuration lifetime and drops replaced account snapshots through normal Go object replacement. V0.1 fixes the request origin to `https://api.z.ai`: custom quota endpoints are disabled in production configuration. The client rejects redirects instead of forwarding `Authorization`, does not honor ambient HTTP proxy variables by default, sends no body, caps the response body at 1 MiB, applies an overall request timeout, validates the JSON schema and status code, and never logs raw headers or bodies.

Premium credits are discounted to 0.5× off peak; peak is Monday through Friday, 06:00–10:00 UTC. Expose an `offpeak` status flag computed from an injected UTC clock. Apply the 0.5× multiplier only to fallback credit events timestamped off peak; do not alter authoritative utilization to re-derive Z.AI's accounting.

### Fallback formula

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

- bind published cached-input pricing to `UsageDetail.CacheReadTokens` only;
- bind cache writes to `UsageDetail.CacheCreationTokens` and charge them at the normal input rate unless Z.ai documents a separate rate;
- never use generic `UsageDetail.CachedTokens` for pricing because the v7.2.67 Claude usage helper can substitute cache-creation tokens into that field when it is zero;
- subtract `CacheReadTokens` from `InputTokens` only when a redacted real fixture proves that CPA's input count includes cache reads; otherwise treat the fields as disjoint and fail the contract test rather than double-subtracting;
- a failed request with no usage adds no estimated credits; and
- a failure that includes usage is accounted on best effort.

`usage.handle` is a lossy observation channel, not an authoritative ledger: its interface has no acknowledgment, CPA discards RPC errors, delivery may be skipped after request-context cancellation, and `UsageRecord` contains no request ID. The estimator therefore reports an integrity state (`complete_since`, `delivery_warning`, and `dedup_mode`) and becomes conservative when persistence or delivery uncertainty is observed; it never claims exact consumption and never clears an upstream 429. A bounded hash of stable non-secret fields may suppress obvious replays across restart/reconfigure, but collisions can suppress legitimate identical records, so hash dedup is explicitly heuristic rather than replay-safe.

### Fallback rolling windows

Maintain timestamped credit events per account only while authoritative quota is unavailable:

- five-hour: `(now - 5h, now]`;
- weekly: `(now - 7d, now]`.

These local windows are a conservative fallback. They do not override Z.AI's returned `nextResetTime` or bucket size.

For each fallback window:

```text
utilization = consumed_credits / configured_bucket
resets_at   = earliest expiry after which utilization is below threshold
```

When fallback usage is over threshold, `resets_at` is not necessarily `oldest + duration`: walk expiry events until the remaining sum becomes strictly lower than the configured threshold. Inject a clock so boundaries are deterministic.

Persist fallback state with a same-directory temporary write, file `fsync`, rename, and directory `fsync` where supported.

## Health state

Shared account health values:

- `healthy` — valid, enabled, under both thresholds, no active upstream block;
- `exhausted` — local threshold reached or 429 block active;
- `suspended` — 401/403 block active;
- `disabled` — administratively disabled;
- `config_error` — invalid pair/settings.

| Signal | Result |
|---|---|
| Fresh authoritative quota below thresholds | Healthy after any transient failure timer expires. |
| Either authoritative bucket reaches threshold | Exhaust until its returned reset or a successful refresh below threshold. |
| Quota endpoint unavailable | Retain bounded fresh data, then use the clearly labelled local estimator. |
| Either fallback estimate reaches threshold | Exhaust until the fallback rolling sum falls below threshold. |
| HTTP 429 | Exhaust; parse bounded reset hints or use fallback cooldown. |
| HTTP 401/403 | Suspend for configured duration. |
| Window/timer expiry | Recompute and clear automatically. |
| Management unblock | Clear transient flags and recompute retained quota/usage; never erase consumption. |
| Key rotation | Preserve state only for the same hashed identity. |

A real 429 overrides a lower estimate. Repeated failures extend, never shorten, an active block. Treat reset hints as untrusted: bound body/header lengths, reject malformed/past/unreasonably distant values, and expose the chosen reason/reset.

## Scheduler

CPA v7.2.67 invokes only the first active scheduler plugin, ordered by descending `plugins.configs.<id>.priority` and then ascending plugin ID. Quota enforcement is therefore a deployment invariant, not composable middleware: v0.1 supports exactly one enabled scheduler plugin, `zai-coding-plan`. Startup/dogfood validation inspects the configured and registered capability set and refuses the Z.ai lane if any second scheduler is enabled or if this plugin is not first. `priority: 1000` is required as defense in depth, but exclusivity is the safety property. CI fixtures cover a lower-priority competitor, a higher-priority competitor, and an equal-priority ID tie; every non-exclusive configuration must fail closed before traffic is admitted.

For `scheduler.pick`:

1. Map candidates to logical accounts and reject the call with a scheduler error if any recognized Z.ai candidate is absent from the validated snapshot.
2. Recompute expired health.
3. If no managed account is impaired, return `Handled:true, DelegateBuiltin:"round-robin"`; CPA retains native round-robin/session affinity through the explicit SDK delegate.
4. In degraded state, discard every candidate belonging to an impaired account, including its sibling credential.
5. Round-robin among healthy candidates per provider/model, preserving header-derived stickiness when possible (`X-Session-ID`, `Session-Id`, `Session_id`, `X-Client-Request-Id`). Return the selected `AuthID` with `Handled:true`.
6. If the request contains managed Z.ai candidates but no healthy managed candidate remains, return a non-retryable scheduler error such as `zai_no_capacity`. In v7.2.67, returning `Handled:false` falls back to built-in selection and is forbidden on this path.
7. If the request contains no recognized Z.ai candidate, return `Handled:false` so unrelated providers remain untouched.

The scheduler performs no disk, network, or host callback while holding its lock. Contract tests assert the all-impaired plugin error propagates through `pluginhost.PickAuth` with `handled=true` and cannot fall back to a known-bad credential.

## Management API

`management.register` adds routes mounted by CPA under `/v0/management` and protected by CPA's management key:

| Method/path | Purpose |
|---|---|
| `GET /v0/management/plugins/zai-coding-plan/status` | Redacted utilization/reset state, quota source and freshness, off-peak state, health, and warnings. |
| `POST /v0/management/plugins/zai-coding-plan/refresh` | Poll quota now, compact fallback state, and recompute health. |
| `POST /v0/management/plugins/zai-coding-plan/unblock` | Clear transient blocks, then recompute retained quota/usage. |
| `POST /v0/management/plugins/zai-coding-plan/account-config` | Save/clear non-key plan metadata. |

Collector-facing status is locked by `src/testdata/status_authoritative.golden.json` and `src/testdata/status_fallback.golden.json`. Utilizations are finite ratios in `[0,1]`; `quota_age_seconds` is a non-negative integer; timestamp fields are RFC 3339; `quota_source` is `quota_api` or `estimate`; and unknown top-level or account fields fail closed. Freshness, reset, off-peak, health, and estimator-integrity fields are machine-visible. `quota_error` is omitted when empty and otherwise contains only a bounded redacted message. Status never contains full keys, derived account identities, request bodies, authorization headers, or management credentials. A retained snapshot returned with `status=reconfigure_rejected` is projected only as stale `config_error` capacity and cannot be selected as healthy capacity; live installation verification rejects that state outright.

The host collector writes its separate sanitized lane file only beneath a pre-created root-owned real directory. It rejects symlinked or substituted output parents, holds an `O_DIRECTORY|O_NOFOLLOW` descriptor across mode enforcement, same-directory temporary creation, atomic replacement, and directory fsync, and writes the final file as mode `0600`. This root-owned collector output is distinct from the unprivileged CPA state directory below.

CPA must reject unauthenticated management HTTP requests before dispatch. If a resource route provides UI, it serves only a static shell; data still comes from the authenticated management endpoint.

## Persistence and concurrency

```text
<auth-dir>/zai-coding-plan/
├── settings.json
├── state.json
└── settings.recovery.json  # transient during settings commit recovery
```

Requirements:

- directory mode `0700`;
- files mode `0600`, including replacements;
- reject symlink targets and use atomic same-directory replacement;
- never persist the API key;
- key settings by hashed account identity so renames survive but rotations reset;
- one in-process mutex protects the active snapshot, state, dedup set, and cursors;
- copy a persistence snapshot under lock, then write after releasing it; and
- shutdown is idempotent and ordered: stop accepting new work, cancel the quota-poller context, synchronously join every poller/refresh worker, then flush persistence and return. No goroutine may retain or call the host callback table after shutdown returns, because CPA deletes host callback state and immediately unloads the `.so`.

Quarantine corrupt state with a redacted diagnostic. Start conservatively with an accounting warning rather than silently replacing it.

## Data flow

```text
CPA config YAML
    │ plugin.register / plugin.reconfigure
    ▼
validate ── discover exact key pairs ── load 0600 state/settings
    │
    ├── both auth IDs ─────────────────────────────┐
    │                                              │
quota poller ── authoritative buckets/resets        │ CPA candidates
    │                                              │ scheduler.pick
    ├── unavailable → local estimator               ▼
CPA completed request ── usage.handle ── health ── recompute health
    │                                  │            │
    ├── usage → fallback rolling ledger│            ├── all healthy → delegate builtin RR
    └── 401/403/429 ───────────────────┘            ├── degraded → healthy candidate
                                                    └── none healthy → scheduler error
    │
    ├── atomic redacted persistence
    └── authenticated status → collector lane `zai`
```

## Threat model

### Assets

Z.ai keys, CPA management authentication, usage/account state, local ledger integrity, and the native `.so` executing in CPA.

| Threat | Control |
|---|---|
| Scheduler precedence bypasses quota enforcement | Require `zai-coding-plan` to be the only enabled scheduler, validate the configured/registered capability set before admitting the lane, and retain `priority: 1000` only as defense in depth. |
| Keys leak through logs/status/errors | Keys live only in the active in-memory account snapshot for exact pairing and authenticated quota requests; they are never persisted or returned. Central redaction and tests scan every serialized output/log/error path for fixture keys. |
| Quota request leaks or is redirected | Fix production requests to HTTPS `api.z.ai`, reject redirects, disable ambient proxy use by default, never forward credentials cross-origin, cap body/time, validate schema, and never log raw headers or bodies. |
| Wrong pairing through suffix collision | Pair only by full-key equality. Suffix matching is override-only and ambiguity fails closed. |
| Unauthenticated quota/account disclosure | Data only on CPA management-key routes. Integration-test unauthorized/authorized HTTP behavior. Resource shell has no data. |
| Local disclosure | `0700` directory, `0600` files, unprivileged CPA user, no secrets in filenames. |
| Ledger tampering creates false capacity | Validate schema, non-negative bounded values, model/rate IDs, timestamps, sizes. Corruption causes conservative degraded state. Unblock never deletes usage. |
| ABI mismatch crashes CPA | Pin v7.2.67 SDK, inspect exported symbol, then load-test exact eceasy image digest. |
| Malicious reset hints deny service | Allowlist fields, cap lengths/future duration, reject malformed/past values. |
| Usage replay double charges | Persist a bounded heuristic hash of stable non-secret fields; surface collision/replay limitations through `dedup_mode` and integrity warnings. |
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

- quota response parsing for Pro/Lite/Max buckets, reset epochs, malformed/oversized input, endpoint failures, and off-peak boundaries;
- quota client redirect rejection, fixed-origin enforcement, disabled ambient proxies, body/time caps, secret redaction, and cancellation;
- poll jitter, timeout, stale-data, authoritative-to-fallback-to-authoritative reconciliation, and freshness fields;
- exact fallback formula vectors, large counts, zero usage, and rounding boundaries;
- cache semantics and unknown models;
- fallback rolling boundary and earliest threshold reset with multiple events;
- Lite/Pro/Max/custom bucket validation;
- exact pairing, duplicates, missing siblings, ambiguous suffixes, rotation;
- reset headers/body variants and adversarial values;
- cross-protocol 401/403/429 health;
- scheduler precedence/exclusivity with lower-, higher-, and equal-priority competitors; healthy explicit built-in round-robin delegation, degraded exclusion/round-robin/stickiness, and all-impaired hard-error propagation;
- invalid reconfiguration retains the previous internal snapshot, returns `invalid_config`, surfaces the validation error, and is withdrawn from the host capability snapshot;
- lossy usage delivery, persistence-failure integrity state, and heuristic dedup collision/replay cases;
- shutdown cancels and joins every background worker before callback teardown/unload;
- permissions, atomic replacement, corrupt files, symlinks; and
- collector status golden JSON plus adversarial strict-schema, RFC JSON, timestamp, integer-age, rejected-reconfiguration, one-line-key, and output-parent substitution probes.

ABI/integration coverage:

- Go 1.26 CGO `.so` build and exported symbol;
- upstream v7.2.67 load baseline;
- exact deployed eceasy image load;
- paired-account synthetic usage/failure flow;
- management-key enforcement; and
- persistence across shutdown/reload without double count.

## Operational and ToS note

The Z.ai GLM Coding Plan is licensed for supported coding tools, and Claude Code is one of those tools. Our intended agent workloads are Claude Code sessions. Deployment must enforce that boundary outside the plugin: expose the `zai` lane only to the authenticated Claude Code agent environment, do not publish it as a general OpenAI-compatible endpoint, and do not route arbitrary clients through the plan credentials. The plugin classifies credentials and capacity; it cannot itself prove which end-user tool originated generic CPA traffic. Re-check current Z.ai terms before any release that materially changes clients or traffic patterns.

The repository and reference plugin are MIT-licensed. Preserve required notices when adapting reference code.

## Open verification items before v0.1.0

1. Capture a redacted real quota response fixture and verify the five-hour/weekly unit mapping and utilization scale against the live endpoint.
2. Capture redacted real `pluginapi.UsageRecord` fixtures to validate input/cache field overlap and model aliases for fallback accounting. Pricing is already bound to `CacheReadTokens` and `CacheCreationTokens`, never generic `CachedTokens`.
3. Capture real redacted Z.ai 429/reset variants.
