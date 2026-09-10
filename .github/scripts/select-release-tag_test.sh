#!/usr/bin/env bash
set -euo pipefail

script=.github/scripts/select-release-tag.sh
release_sha=$(git rev-parse 'v0.1.0^{commit}')
main_sha=$(git rev-parse HEAD)

test "$("$script" push refs/tags/v0.1.0 "$release_sha" '')" = v0.1.0
test "$("$script" workflow_run refs/heads/main "$main_sha" "$main_sha")" = v0.1.0

if "$script" workflow_run refs/heads/main "$main_sha" "$release_sha" >/dev/null 2>&1; then
  printf 'accepted mismatched workflow and upstream SHAs\n' >&2
  exit 1
fi
if "$script" push refs/heads/main "$main_sha" '' >/dev/null 2>&1; then
  printf 'accepted unsupported branch push\n' >&2
  exit 1
fi
