# Security Policy

## Reporting

Report vulnerabilities privately through GitHub Security Advisories. Do not open a public issue for a suspected vulnerability.

## Supported versions

Until the first stable release, only the latest tagged release receives security fixes.

## Credential handling

Never commit or log API keys. Plugin settings containing secrets must be stored under `<auth-dir>/zai-coding-plan/` with file mode `0600`. Any management endpoint must authenticate with the CLIProxyAPI management key.
