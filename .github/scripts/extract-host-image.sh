#!/usr/bin/env bash
set -euo pipefail

pin="${1:-.github/release-host-image.json}"
out="${2:?output directory is required}"
repository=$(jq -er .repository "$pin")
digest=$(jq -er .manifest_digest "$pin")
platform=$(jq -er .platform "$pin")
[[ "$platform" == "linux/amd64" ]]

auth=$(curl --fail --location --silent --show-error \
  "https://auth.docker.io/token?service=registry.docker.io&scope=repository:${repository}:pull" | jq -er .token)
mkdir -p "$out/rootfs"
manifest="$out/manifest.json"
curl --fail --location --silent --show-error \
  -H "Authorization: Bearer $auth" \
  -H 'Accept: application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json' \
  "https://registry-1.docker.io/v2/${repository}/manifests/${digest}" > "$manifest"

resolved=$(sha256sum "$manifest" | cut -d' ' -f1)
[[ "sha256:${resolved}" == "$digest" ]]

index=0
while IFS= read -r layer; do
  [[ "$layer" =~ ^sha256:([0-9a-f]{64})$ ]]
  expected="${BASH_REMATCH[1]}"
  index=$((index + 1))
  blob="$out/layer-${index}.tar.gz"
  curl --fail --location --silent --show-error \
    -H "Authorization: Bearer $auth" \
    "https://registry-1.docker.io/v2/${repository}/blobs/${layer}" > "$blob"
  actual=$(sha256sum "$blob" | cut -d' ' -f1)
  [[ "$actual" == "$expected" ]]
  tar -xzf "$blob" -C "$out/rootfs" --overwrite --exclude='dev/*'
done < <(jq -er '.layers[].digest' "$manifest")

host="$out/rootfs/CLIProxyAPI/CLIProxyAPI"
test -x "$host"
printf '%s\n' "$host"
