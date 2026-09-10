#!/usr/bin/env bash
set -euo pipefail

script=.github/scripts/select-release-tag.sh
release_sha=$(git rev-parse 'v0.1.0^{commit}')
main_sha=$(git rev-parse HEAD)
control_directory=$(mktemp -d)
trap 'rm -rf "$control_directory"' EXIT

git clone -q --no-hardlinks . "$control_directory"
git -C "$control_directory" checkout -q --detach "$main_sha"
test ! -e "$control_directory/release-source"

test "$("$script" push refs/tags/v0.1.0 "$release_sha" '' .)" = v0.1.0
test "$("$script" workflow_run refs/heads/main "$main_sha" "$main_sha" "$control_directory")" = v0.1.0

git -C "$control_directory" worktree add -q --detach "$control_directory/release-source" v0.1.0
test -x "$control_directory/.github/scripts/select-release-tag.sh"
test ! -e "$control_directory/release-source/.github/scripts/select-release-tag.sh"
test "$(git -C "$control_directory/release-source" rev-parse HEAD)" = "$release_sha"

if "$script" workflow_run refs/heads/main "$main_sha" "$release_sha" . >/dev/null 2>&1; then
  printf 'accepted mismatched workflow and upstream SHAs\n' >&2
  exit 1
fi
if "$script" push refs/heads/main "$main_sha" '' . >/dev/null 2>&1; then
  printf 'accepted unsupported branch push\n' >&2
  exit 1
fi
