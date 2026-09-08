# Contributing

Use a branch and pull request for every change. Commits follow Conventional Commits and releases follow semantic versioning.

Before opening a pull request, run:

```sh
make fmt-check
make vet
make test
golangci-lint run ./...
make build
```

Update `CHANGELOG.md` for user-visible changes. Do not commit credentials, generated shared libraries, or private configuration. Security-sensitive changes require security review.
