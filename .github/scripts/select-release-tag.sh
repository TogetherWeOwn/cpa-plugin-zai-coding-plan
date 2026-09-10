#!/usr/bin/env bash
set -euo pipefail

readonly recovery_ref=refs/heads/release-recovery/v0.1.0
readonly recovery_tag=v0.1.0
readonly recovery_sha=5c758c04acbd9367d1c8fff1342bf651847dcac2

event_ref=${1:?usage: select-release-tag.sh EVENT_REF EVENT_SHA}
event_sha=${2:?usage: select-release-tag.sh EVENT_REF EVENT_SHA}

case "$event_ref" in
  refs/tags/v*)
    raw_tag=${event_ref#refs/tags/}
    test "$(git rev-parse "$raw_tag^{commit}")" = "$event_sha"
    ;;
  "$recovery_ref")
    git fetch --no-tags origin main
    test "$(git rev-parse origin/main)" = "$event_sha"
    raw_tag=$recovery_tag
    test "$(git rev-parse "$raw_tag^{commit}")" = "$recovery_sha"
    ;;
  *)
    printf 'unsupported release ref: %s\n' "$event_ref" >&2
    exit 1
    ;;
esac

printf '%s\n' "$raw_tag"
