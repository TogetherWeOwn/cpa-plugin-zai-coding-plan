## Summary

<!-- What changed, why, and what user or operator behavior is affected? -->

## Verification

- [ ] `make fmt-check`
- [ ] `make vet`
- [ ] `go test -race ./...`
- [ ] `make test-release`
- [ ] `make validate-source`
- [ ] `make lint`
- [ ] `make package VERSION=<version>` when packaging changes

## Contract and release checks

- [ ] The change follows `docs/ARCHITECTURE.md`, or the deviation is called out and approved before implementation.
- [ ] User-visible behavior updates README/config/API documentation and `CHANGELOG.md`.
- [ ] Registry, archive names, version stamping, and checksums remain machine-consistent.
- [ ] No credentials, authorization headers, private configuration, or generated release artifacts are committed.
- [ ] Native ABI changes include an exported-symbol check and immutable CLIProxyAPI image load evidence.

## Exact-SHA review

Head SHA submitted for review: `________________________________________`

- [ ] Normal code review is bound to the head SHA above.
- [ ] A new commit after review triggers a new exact-SHA review.
- [ ] Credential, persistence, quota-request, or management changes have a separate security review bound to the same SHA.

## Risk and rollback

<!-- Describe failure modes and the exact revert/rollback procedure. -->
