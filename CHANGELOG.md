# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

> This entry describes the v0.1.0 release candidate. The release workflow refuses publication unless the tag, registry, archive names, and checksums match this version at the reviewed release commit.

[Unreleased]: https://github.com/TogetherWeOwn/cpa-plugin-zai-coding-plan/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/TogetherWeOwn/cpa-plugin-zai-coding-plan/releases/tag/v0.1.0
