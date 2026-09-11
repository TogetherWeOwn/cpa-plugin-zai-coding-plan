#!/usr/bin/env bash
set -euo pipefail

script=.github/scripts/select-release-tag.sh
release_sha=$(git rev-parse 'v0.2.0^{commit}')
main_sha=$(git rev-parse HEAD)
control_directory=$(mktemp -d)
trap 'rm -rf "$control_directory"' EXIT

git clone -q --no-hardlinks . "$control_directory"
git -C "$control_directory" checkout -q --detach "$main_sha"
test ! -e "$control_directory/release-source"

test "$("$script" push refs/tags/v0.2.0 "$release_sha" .)" = v0.2.0

if "$script" push refs/heads/main "$main_sha" . >/dev/null 2>&1; then
  printf 'accepted unsupported branch push\n' >&2
  exit 1
fi
if "$script" push refs/tags/v0.2.0 "$main_sha" . >/dev/null 2>&1; then
  printf 'accepted mismatched tag object\n' >&2
  exit 1
fi
