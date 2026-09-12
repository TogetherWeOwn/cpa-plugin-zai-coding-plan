#!/usr/bin/env bash
set -euo pipefail

event_name=${1:?usage: select-release-tag.sh EVENT_NAME EVENT_REF EVENT_SHA [GIT_DIRECTORY]}
event_ref=${2:?usage: select-release-tag.sh EVENT_NAME EVENT_REF EVENT_SHA [GIT_DIRECTORY]}
event_sha=${3:?usage: select-release-tag.sh EVENT_NAME EVENT_REF EVENT_SHA [GIT_DIRECTORY]}
git_directory=${4:-.}

resolve() {
  git -C "$git_directory" rev-parse "$1"
}

case "$event_name" in
  push)
    case "$event_ref" in
      refs/tags/v*) raw_tag=${event_ref#refs/tags/} ;;
      *)
        printf 'unsupported release ref: %s\n' "$event_ref" >&2
        exit 1
        ;;
    esac
    test "$(resolve "$raw_tag^{commit}")" = "$event_sha"
    ;;
  *)
    printf 'unsupported release event: %s\n' "$event_name" >&2
    exit 1
    ;;
esac

printf '%s\n' "$raw_tag"
