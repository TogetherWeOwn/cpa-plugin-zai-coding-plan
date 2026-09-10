#!/usr/bin/env bash
set -euo pipefail

: "${CLIPROXY_MANAGEMENT_URL:=http://127.0.0.1:8317}"
: "${CLIPROXY_USAGE_DIR:=/srv/cliproxy-usage}"
: "${CLIPROXY_MANAGEMENT_KEY_FILE:?set CLIPROXY_MANAGEMENT_KEY_FILE to a root-readable 0600 file}"

status_url="${CLIPROXY_MANAGEMENT_URL%/}/v0/management/plugins/zai-coding-plan/status"
management_key=$(<"$CLIPROXY_MANAGEMENT_KEY_FILE")
test -n "$management_key"

unauthenticated=$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' "$status_url")
test "$unauthenticated" = 401

status_file=$(mktemp)
trap 'rm -f "$status_file"' EXIT
curl --fail-with-body --silent --show-error \
  -H "Authorization: Bearer ${management_key}" \
  "$status_url" >"$status_file"

python3 - "$status_file" <<'PY'
import json, re, sys
status=json.load(open(sys.argv[1]))
required={"name","key_suffix","plan","five_hour_utilization","weekly_utilization","five_hour_resets_at","weekly_resets_at","quota_source","quota_observed_at","quota_age_seconds","quota_stale","offpeak","health","estimator_complete_since","delivery_warning","persistence_warning","unknown_model_warning","heuristic_dedup_warning","dedup_mode"}
assert status["plugin"]=="zai-coding-plan" and status["status"]=="registered"
assert status["accounts"]
for account in status["accounts"]:
    assert required <= set(account)
    assert account["key_suffix"] == "redacted"
    assert account["quota_source"] in {"quota_api","estimate"}
    assert 0 <= account["five_hour_utilization"] <= 1
    assert 0 <= account["weekly_utilization"] <= 1
    assert not any(re.search(r"(?:api[-_]?key|authorization|credential|secret|token|key_hash|identity)", key, re.I) for key in account)
PY

"$(dirname "$0")/collector-zai.py" \
  --management-key-file "$CLIPROXY_MANAGEMENT_KEY_FILE" \
  --url "$status_url" \
  --output "$CLIPROXY_USAGE_DIR/zai.json"

python3 - "$CLIPROXY_USAGE_DIR/zai.json" <<'PY'
import json, re, sys
payload=json.load(open(sys.argv[1]))
assert payload["schemaVersion"]==1 and payload["lane"]=="zai" and payload["records"]
raw=json.dumps(payload)
assert not re.search(r'"(?:api[-_]?key|authorization|credential|secret|token|key_hash|identity)"\s*:', raw, re.I)
assert all(record["key_suffix"]=="redacted" for record in payload["records"])
PY

printf 'dogfood-live: authenticated status field contract, zai collector lane, and redaction PASS\n'
