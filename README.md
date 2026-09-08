# cpa-plugin-zai-coding-plan

A CLIProxyAPI plugin for Z.AI Coding Plan. This repository currently contains the production-ready scaffold and C ABI registration layer; provider behavior will follow in a separate architecture-led change.

## Requirements

- Linux amd64
- Go 1.26
- GCC-compatible C toolchain (CGO)
- CLIProxyAPI v7.2.x

## Build

```sh
make test
make build
```

The default build writes `dist/zai-coding-plan-v0.0.1.so`. `make package` produces the release archive and SHA-256 checksums.

## Install

Copy the versioned shared object into CLIProxyAPI's `plugins/linux/amd64/` directory, add the plugin configuration from `config.example.yaml`, and restart CLIProxyAPI. Use `make deploy DEPLOY_DIR=/path/to/plugins/linux/amd64` to copy the artifact. Restart CLIProxyAPI after deploying a new plugin build.

## Configuration

The plugin reads its settings from `plugins.configs.zai-coding-plan` and projects the CPA provider entries from `cpa-config-path`. It manages only exact-key pairs consisting of a Z.ai Anthropic entry and an OpenAI-compatible provider named `zai-coding-plan`; suffixes are used only for account metadata overrides. See `config.example.yaml`.

Local settings and state are stored atomically under `<auth-dir>/zai-coding-plan/`. The directory is mode `0700`, files are mode `0600`, and provider keys are never persisted.

## API and security

The scaffold advertises no scheduler, usage, or management capability. When management endpoints are added, they must require the CLIProxyAPI management key. Never commit or log API keys.

## Development

See [CONTRIBUTING.md](CONTRIBUTING.md), [SECURITY.md](SECURITY.md), and [CHANGELOG.md](CHANGELOG.md).
