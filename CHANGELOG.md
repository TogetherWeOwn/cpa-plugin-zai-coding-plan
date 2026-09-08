# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

- Advertise `scheduler`, `usage_plugin`, and `management_api` capabilities at registration. CLIProxyAPI v7.2.67 rejects plugins that advertise no capability, so the previous scaffold could never become an active plugin.
- The scheduler capability declines every pick (`Handled: false`), keeping the host's native scheduler in control, and a read-only `GET /v0/management/plugins/zai-coding-plan/status` route is registered behind the management key.

### Added

- Pinned-host integration test that builds the plugin as a c-shared library and loads it through `sdk/pluginhost`, asserting the plugin appears in the host registration snapshot.

## [0.0.1] - 2026-09-08

### Added

- Initial CLIProxyAPI C ABI plugin scaffold.
- CI, release packaging, registry metadata, project documentation, and community templates.

[Unreleased]: https://github.com/TogetherWeOwn/cpa-plugin-zai-coding-plan/compare/v0.0.1...HEAD
[0.0.1]: https://github.com/TogetherWeOwn/cpa-plugin-zai-coding-plan/releases/tag/v0.0.1
