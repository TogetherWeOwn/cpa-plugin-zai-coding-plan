#!/usr/bin/env bash
set -euo pipefail

script=.github/scripts/select-release-tag.sh
release_sha=$(git rev-parse 'v0.1.0^{commit}')
git fetch --no-tags origin main
main_sha=$(git rev-parse origin/main)

test "$("$script" refs/tags/v0.1.0 "$release_sha")" = v0.1.0
test "$("$script" refs/heads/release-recovery/v0.1.0 "$main_sha")" = v0.1.0

if "$script" refs/heads/release-recovery/v0.1.0 "$release_sha" >/dev/null 2>&1; then
  printf 'accepted recovery ref that did not match origin/main\n' >&2
  exit 1
fi
if "$script" refs/heads/main "$main_sha" >/dev/null 2>&1; then
  printf 'accepted unsupported branch ref\n' >&2
  exit 1
fi
