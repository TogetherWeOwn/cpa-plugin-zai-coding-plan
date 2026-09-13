# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed

- The `/v0/resource/plugins/subscription-pool/status` page (Management Center's "Plugins" nav entry) is now a real per-account dashboard instead of a static placeholder: it renders both `zai` and `opencode-go` accounts server-side from the coordinator's own `Status()` aggregation, with per-window utilization bar, percent, absolute UTC reset timestamp, countdown, source (quota_api/estimate for Z.ai; authoritative/429-derived/unknown for OpenCode Go), and health. No management or dashboard key is ever embedded in the page. Resource-page ownership moved from the `zai` module to the coordinator, since only the coordinator can see both providers; the menu entry is renamed from "Z.ai Quota" to "Subscription Quota" and its description now mentions both providers. (TOG-2477)

## [0.4.1] - 2026-09-13

### Fixed

- `providers.opencode-go`: the dashboard usage poller fetched `/zen/go/v1/usage` once with a single module-level key and broadcast that one snapshot to every configured account, so all accounts reported identical utilization/reset regardless of their real usage. `dashboard-api-key` now lives on `accounts[]` (falling back to the module-level key only when an account omits its own), and each account is polled independently with its own key. (TOG-2472)
- `providers.zai`: the quota poller rejected upstream CREDIT_LIMIT/`nextResetTime` values serialized with a trailing `.0` (e.g. `1789347583607.0`), falling back to a stuck-at-0% estimate. `strictInt64` now also accepts an exact, finite, in-range whole-number float. (TOG-2473)

## [0.4.0] - 2026-09-13

### Added

- Operator bundle now packages the OpenCode Go telemetry path alongside the existing Z.ai lane: `collector-opencodego.py` and `deploy/verify-live-opencodego.sh` are installed into the release bundle, checksummed, and enforced by strict bundle-manifest validation.
- Release workflow artifact staging and the published GitHub release now include both provider collectors and both live verifiers, so the installed bundle proves both `zai` and `opencode-go` provider modules are present and their telemetry files are valid.

### Changed

- Versioned release metadata (`Makefile`, `registry.json`, CI/package expectations, deploy runbook text) bumped to `0.4.0` to reflect the dual-provider `subscription-pool` coordinator baseline merged in `0.3.0`.

## [0.3.0] - 2026-09-13

### Added

- Neutral `subscription-pool` coordinator plugin ID replacing the single-provider `zai-coding-plan` registration; Z.ai support is now a `providerModule` extraction (`internal/providers/zai`) behind a coordinator-owned dispatch registry.
- `providerModule` interface and per-`authID`/`providerID`/`logicalAccountID` dispatch registry: zero recognized families yields `Handled:false`, more than one recognized family or a recognized-but-unmanaged/all-impaired family fails closed instead of silently passing traffic through.
- Config YAML reshaped to a `providers.<name>` root shape (`providers.zai`, with `providers.opencode-go` reserved for the sibling module) with strict unknown-field rejection.

### Changed

- Z.ai account/quota/health/polling/management behavior is unchanged; existing tests were relocated to `internal/providers/zai` and continue to pass without modification.

## [0.2.0] - 2026-09-11

### Added

- v0.2.0 release packaging and strict archive/checksum validation for the Linux/amd64 shared library and plugin-store archive.
- Deployed, latest, and CLIProxyAPI v7.2.67 baseline compatibility evidence in the release gate.
- Credential-free live verification, bounded canary guidance, and recoverable rollback procedures for the Z.ai lane.

### Security

- Live verification rejects malformed or confidential management responses, unsafe key-file permissions, missing managed capacity, and unbounded service output.
- Rollback validates the backup and safe configuration directory before management-plane mutation and atomically restores the configuration.

## [0.1.0] - 2026-09-10

### Added

- Exact-key pairing of Z.AI Anthropic and OpenAI-compatible CLIProxyAPI credentials into one logical account.
- Authoritative five-hour and weekly quota polling, including reset timestamps and off-peak status, with fixed-point token accounting as a degraded fallback.
- Shared account health and quota-aware scheduling across both credential protocols.
- Authenticated management status, refresh, unblock, and non-secret account-configuration operations.
- Atomic redacted state storage under `<auth-dir>/zai-coding-plan/` with restrictive permissions.
- Native CLIProxyAPI v7.2.x C ABI registration for `scheduler`, `usage_plugin`, and `management_api` capabilities.
- Pinned-host integration tests covering registration, management dispatch, quota-aware scheduling, invalid reconfiguration, and all-impaired failure propagation.
- Versioned Linux/amd64 shared-library and plugin-store archive packaging with SHA-256 checksums.
- Continuous integration, exact-image compatibility gate, release consistency checks, security checks, and community documentation.

### Security

- Management routes rely on CLIProxyAPI management-key authentication.
- Raw plan keys, authorization headers, request bodies, and management credentials are excluded from persistent state, logs, and status responses; persistence uses only derived account identities and redacted state.
- State and settings commits are atomic, redacted, and recovered fail-closed after interrupted writes.

[Unreleased]: https://github.com/TogetherWeOwn/cpa-plugin-zai-coding-plan/compare/v0.4.1...HEAD
[0.4.1]: https://github.com/TogetherWeOwn/cpa-plugin-zai-coding-plan/releases/tag/v0.4.1
[0.4.0]: https://github.com/TogetherWeOwn/cpa-plugin-zai-coding-plan/releases/tag/v0.4.0
[0.3.0]: https://github.com/TogetherWeOwn/cpa-plugin-zai-coding-plan/releases/tag/v0.3.0
[0.2.0]: https://github.com/TogetherWeOwn/cpa-plugin-zai-coding-plan/releases/tag/v0.2.0
[0.1.0]: https://github.com/TogetherWeOwn/cpa-plugin-zai-coding-plan/releases/tag/v0.1.0
