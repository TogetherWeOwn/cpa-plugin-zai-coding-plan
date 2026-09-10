#!/usr/bin/env bash
set -euo pipefail

scanner="${1:?gitleaks binary is required}"
root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT

mkdir -p "$root/clean"
printf 'runtime supplies credentials\n' > "$root/clean/config.txt"
"$scanner" dir --redact=100 --no-banner "$root/clean" >/dev/null

mkdir -p "$root/tree"
printf 'api_%s = "%s%s"\n' 'key' 'aB3dE5fG7hJ9kL2m' 'N4pQ6rS8tU1vW3xY' > "$root/tree/leak.txt"
if "$scanner" dir --redact=100 --no-banner "$root/tree" >/dev/null 2>&1; then
  printf 'gitleaks failed to detect a generic current-tree credential fixture\n' >&2
  exit 1
fi

mkdir -p "$root/history"
git -C "$root/history" init -q
git -C "$root/history" config user.name release-validation
git -C "$root/history" config user.email release-validation@example.invalid
printf 'api_%s = "%s%s"\n' 'key' 'hJ7kL9mN2pQ4rS6t' 'V8wX1yZ3aB5cD7eF' > "$root/history/leak.txt"
git -C "$root/history" add leak.txt
git -C "$root/history" commit -qm fixture
printf 'removed\n' > "$root/history/leak.txt"
git -C "$root/history" commit -qam removed
if "$scanner" git --redact=100 --no-banner --log-opts=--all "$root/history" >/dev/null 2>&1; then
  printf 'gitleaks failed to detect a historical credential fixture\n' >&2
  exit 1
fi

printf 'gitleaks regression: current-tree and historical fixtures detected\n'
