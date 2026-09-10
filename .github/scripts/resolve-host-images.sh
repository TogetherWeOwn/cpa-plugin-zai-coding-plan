#!/usr/bin/env bash
set -euo pipefail

config="${1:-.github/host-images.json}"
deployed="${2:-deploy/deployed-host-image.json}"
repository=$(jq -er .repository "$config")
platform=$(jq -er .platform "$config")
[[ "$platform" == "linux/amd64" ]]

auth=$(curl --fail --location --silent --show-error \
  "https://auth.docker.io/token?service=registry.docker.io&scope=repository:${repository}:pull" | jq -er .token)
registry="https://registry-1.docker.io/v2/${repository}"
accept='application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json'

resolve_tag() {
  local tag=$1 headers body content_type digest
  headers=$(mktemp)
  body=$(mktemp)
  curl --fail --location --silent --show-error \
    -D "$headers" -o "$body" \
    -H "Authorization: Bearer $auth" \
    -H "Accept: $accept" \
    "$registry/manifests/$tag"
  content_type=$(awk 'BEGIN { IGNORECASE=1 } /^content-type:/ { print $2 }' "$headers" | tr -d '\r' | tail -1)
  case "$content_type" in
    application/vnd.oci.image.index.v1+json|application/vnd.docker.distribution.manifest.list.v2+json)
      digest=$(jq -er '.manifests[] | select(.platform.os == "linux" and .platform.architecture == "amd64") | .digest' "$body")
      ;;
    application/vnd.oci.image.manifest.v1+json|application/vnd.docker.distribution.manifest.v2+json)
      digest=$(awk 'BEGIN { IGNORECASE=1 } /^docker-content-digest:/ { print $2 }' "$headers" | tr -d '\r' | tail -1)
      ;;
    *)
      printf 'unsupported manifest content type for %s: %s\n' "$tag" "$content_type" >&2
      return 1
      ;;
  esac
  [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]
  rm -f "$headers" "$body"
  printf '%s\n' "$digest"
}

latest_tag=$(curl --fail --location --silent --show-error \
  -H "Authorization: Bearer $auth" \
  "$registry/tags/list?n=10000" |
  jq -r '.tags[] | select(test("^v[0-9]+\\.[0-9]+\\.[0-9]+$"))' |
  sort -V | tail -1)
[[ "$latest_tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]
latest_digest=$(resolve_tag "$latest_tag")

jq -ce \
  --arg repository "$repository" \
  --arg platform "$platform" \
  --arg latest_tag "$latest_tag" \
  --arg latest_digest "$latest_digest" \
  --slurpfile deployed "$deployed" '
    def image($name; $tag; $digest; $required):
      {name: $name, repository: $repository, platform: $platform, tag: $tag, manifest_digest: $digest, release_required: $required};
    [
      image("deployed"; $deployed[0].tag; $deployed[0].manifest_digest; true),
      image("latest"; $latest_tag; $latest_digest; true),
      image("baseline"; .baseline.tag; .baseline.manifest_digest; false)
    ]
    | if (map(.repository) | unique) != [$repository] then error("deployed image repository differs from matrix repository") else . end
    | if (map(.platform) | unique) != [$platform] then error("deployed image platform differs from matrix platform") else . end
  ' "$config"
