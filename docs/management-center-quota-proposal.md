# Management Center proposal: plugin-backed quota cards

## Finding

CLIProxyAPI v7.2.151 and v7.2.157 have no plugin hook for extending the Management Center Quota grid. A plugin can register authenticated Management API routes and unauthenticated browser resource menus, but it cannot:

- add a `QuotaProviderType` or `QuotaAdapter` to the panel;
- synthesize or mutate an auth-file row;
- publish quota into another credential's `quota.signals`; or
- opt an API-key provider into the host's passive quota observations.

The host exposes `quota` and `model_quotas` only from its in-memory auth registry. `ProviderSupportsQuotaObservation` is hard-coded to `claude` and `codex`, and the Management Center currently ignores those generic fields anyway: the Quota page classifies auth files through five compiled adapters and calls each provider's upstream usage endpoint.

## Proposed upstream change

Add a small plugin-backed quota source alongside the existing auth-file-backed sources. The panel already holds the management key in `apiClient`; it should fetch plugin status itself rather than passing credentials to the unauthenticated plugin iframe.

### 1. Discover the installed plugin

Use the existing authenticated `pluginsApi.list()` response. When an enabled and registered plugin has id `zai-coding-plan`, add one virtual quota entry per account returned by:

```text
GET /plugins/zai-coding-plan/status
```

`apiClient` resolves that path under `/v0/management` and supplies the same Bearer management key used by the rest of the panel.

### 2. Add a Z.ai provider adapter

Extend these compiled unions/maps:

- `src/features/quota/providers/types.ts`: add `zai` and a `zaiQuota` store slice.
- `src/features/quota/providers/index.ts`: register `ZAI_CONFIG` and `ZaiQuotaBody`.
- `src/features/quota/constants.ts`: add `zai` to `QUOTA_TAB_ORDER`.
- `src/features/quota/logic.ts`: classify virtual Z.ai account entries.
- `src/stores/useQuotaStore.ts`: persist and clear Z.ai card state.
- `src/types/quota.ts`: add the status payload and card state types.

The adapter should fetch the plugin status once per batch and select the account represented by the virtual entry. A request-level promise/cache prevents N identical status requests for N accounts.

### 3. Merge virtual entries in QuotaPage

`QuotaPage` currently derives `entries` only from `authFilesApi.list()`. Add a separate plugin-account loader and merge its virtual entries before classification. Do not fabricate files in the host's auth-files API: the plugin owns logical paired accounts, while the underlying Claude/OpenAI API-key records are an implementation detail and may have different host ids.

Suggested virtual entry fields:

```ts
{
  name: account.name,
  type: 'zai',
  provider: 'zai-coding-plan',
  disabled: false,
  runtimeOnly: true,
  pluginAccount: account.name
}
```

Use the account name supplied by the plugin and never expose the key suffix as a primary identifier.

### 4. Render the existing fields

`ZaiQuotaBody` should render:

- plan;
- five-hour utilization and reset;
- weekly utilization and reset;
- health;
- quota source (`quota_api` or `estimate`);
- stale/freshness warning.

The plugin reports utilization as a ratio in `[0,1]`; convert to percent used for the existing quota-meter convention. Use periods of 5 and 168 hours so sorting and the timeline can consume the windows like Claude/Codex.

### 5. Refresh semantics

A single-card refresh may call the existing plugin endpoint:

```text
POST /plugins/zai-coding-plan/refresh
```

Then refetch status. Batch refresh should invoke it once, not once per account. Ordinary card loading may read status without forcing a quota poll, preserving the plugin's bounded background polling and avoiding request amplification.

## Security constraints

- Never put the management key in an iframe URL, query, fragment, HTML body, `postMessage`, or plugin resource response.
- Keep quota fetches in `apiClient`, which already injects the management Bearer header.
- Treat plugin resource routes as unauthenticated and informational only.
- Do not display API-key-backed `account` fields from auth-files; the panel's own type comment notes that these can contain the raw API key.
- Validate unknown/missing fields and clamp utilization before rendering.

## Tests

1. Provider logic parses ratio utilization, resets, plan, health, source and stale flags.
2. Plugin absent/disabled/unregistered produces no virtual entries and no error banner.
3. One status request serves multiple Z.ai cards in a batch.
4. Batch refresh performs one POST plus one GET.
5. Missing/malformed accounts fail only the affected plugin section.
6. Quota sorting and timeline accept Z.ai 5-hour/weekly windows.
7. No management credential is rendered into plugin iframe properties or resource URLs.
8. Existing five providers retain their current auth-file and click-to-load behavior.

## Rollback

The panel change is isolated behind discovery of the enabled `zai-coding-plan` plugin. Reverting the Z.ai adapter/store/entry merge restores the current five-provider page without changing CLIProxyAPI or plugin behavior.
