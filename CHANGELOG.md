# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

[Unreleased]: https://github.com/TogetherWeOwn/cpa-plugin-zai-coding-plan/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/TogetherWeOwn/cpa-plugin-zai-coding-plan/releases/tag/v0.2.0
[0.1.0]: https://github.com/TogetherWeOwn/cpa-plugin-zai-coding-plan/releases/tag/v0.1.0
