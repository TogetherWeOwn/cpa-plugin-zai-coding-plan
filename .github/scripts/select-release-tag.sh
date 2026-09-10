#!/usr/bin/env bash
set -euo pipefail

readonly recovery_tag=v0.1.0
readonly recovery_tag_object=93441174a393b2d7487df954b4f10103742285fb
readonly recovery_sha=5c758c04acbd9367d1c8fff1342bf651847dcac2

event_name=${1:?usage: select-release-tag.sh EVENT_NAME EVENT_REF EVENT_SHA WORKFLOW_RUN_SHA [GIT_DIRECTORY]}
event_ref=${2:?usage: select-release-tag.sh EVENT_NAME EVENT_REF EVENT_SHA WORKFLOW_RUN_SHA [GIT_DIRECTORY]}
event_sha=${3:?usage: select-release-tag.sh EVENT_NAME EVENT_REF EVENT_SHA WORKFLOW_RUN_SHA [GIT_DIRECTORY]}
workflow_run_sha=${4-}
git_directory=${5:-.}

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
  workflow_run)
    test -n "$workflow_run_sha"
    test "$workflow_run_sha" = "$event_sha"
    raw_tag=$recovery_tag
    test "$(resolve "$raw_tag")" = "$recovery_tag_object"
    test "$(resolve "$raw_tag^{commit}")" = "$recovery_sha"
    ;;
  *)
    printf 'unsupported release event: %s\n' "$event_name" >&2
    exit 1
    ;;
esac

printf '%s\n' "$raw_tag"
