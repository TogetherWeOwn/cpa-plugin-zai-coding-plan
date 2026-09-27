#!/usr/bin/env bash
set -euo pipefail

script=.github/scripts/select-release-tag.sh
main_sha=$(git rev-parse HEAD)
control_directory=$(mktemp -d)
trap 'rm -rf "$control_directory"' EXIT

git clone -q --no-hardlinks --no-tags . "$control_directory"
git -C "$control_directory" checkout -q --detach "$main_sha"
test ! -e "$control_directory/release-source"

# The real repo must not carry a v0.2.0 tag before it is actually released, so
# this fixture tag is created only inside the throwaway clone and never pushed
# or persisted anywhere.
release_sha=$(git -C "$control_directory" rev-parse HEAD~1)
git -C "$control_directory" tag v0.2.0 "$release_sha"

test "$("$script" push refs/tags/v0.2.0 "$release_sha" "$control_directory")" = v0.2.0

if "$script" push refs/heads/main "$main_sha" "$control_directory" >/dev/null 2>&1; then
  printf 'accepted unsupported branch push\n' >&2
  exit 1
fi
if "$script" push refs/tags/v0.2.0 "$main_sha" "$control_directory" >/dev/null 2>&1; then
  printf 'accepted mismatched tag object\n' >&2
  exit 1
fi
