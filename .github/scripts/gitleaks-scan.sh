#!/usr/bin/env bash
set -euo pipefail

version="8.28.0"
archive_sha256="a65b5253807a68ac0cafa4414031fd740aeb55f54fb7e55f386acb52e6a840eb"
cache_dir="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/gitleaks-${version}"
binary="${cache_dir}/gitleaks"

if [[ ! -x "$binary" ]]; then
  mkdir -p "$cache_dir"
  archive="${cache_dir}/gitleaks.tar.gz"
  curl --fail --location --silent --show-error \
    "https://github.com/gitleaks/gitleaks/releases/download/v${version}/gitleaks_${version}_linux_x64.tar.gz" \
    --output "$archive"
  printf '%s  %s\n' "$archive_sha256" "$archive" | sha256sum --check --status
  tar -xzf "$archive" -C "$cache_dir" gitleaks
  chmod 0755 "$binary"
fi

baseline="$(dirname "$0")/gitleaks-baseline.json"
"$binary" git --redact=100 --no-banner --log-opts=--all --baseline-path "$baseline" .
"$binary" dir --redact=100 --no-banner --baseline-path "$baseline" .
"$(dirname "$0")/gitleaks-regression.sh" "$binary"
