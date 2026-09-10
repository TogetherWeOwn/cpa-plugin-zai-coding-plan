#!/usr/bin/env python3
"""Project authenticated Z.ai plugin status into the sanitized collector lane."""
from __future__ import annotations

import argparse
import json
import os
import pathlib
import re
import sys
import urllib.error
import urllib.request
from datetime import datetime, timezone
from typing import Any, NoReturn

EXPECTED_ACCOUNT_FIELDS = {
    "name",
    "key_suffix",
    "plan",
    "five_hour_utilization",
    "weekly_utilization",
    "five_hour_resets_at",
    "weekly_resets_at",
    "quota_source",
    "quota_observed_at",
    "quota_age_seconds",
    "quota_stale",
    "quota_error",
    "offpeak",
    "health",
    "estimator_complete_since",
    "delivery_warning",
    "persistence_warning",
    "unknown_model_warning",
    "heuristic_dedup_warning",
    "dedup_mode",
}
SECRET_FIELD = re.compile(r"(?:api[-_]?key|authorization|credential|secret|token|key_hash|identity)", re.I)
ALLOWED_HEALTH = {"healthy", "exhausted", "suspended", "disabled", "config_error"}
ALLOWED_SOURCES = {"quota_api", "estimate"}


def fail(message: str) -> NoReturn:
    raise ValueError(message)


def require_ratio(value: Any, field: str) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)) or not 0 <= float(value) <= 1:
        fail(f"{field} must be a ratio in [0,1]")
    return float(value)


def validate_no_secret_fields(value: Any, path: str = "status") -> None:
    if isinstance(value, dict):
        for key, child in value.items():
            if SECRET_FIELD.search(key) and key != "key_suffix":
                fail(f"secret-like field is forbidden: {path}.{key}")
            validate_no_secret_fields(child, f"{path}.{key}")
    elif isinstance(value, list):
        for index, child in enumerate(value):
            validate_no_secret_fields(child, f"{path}[{index}]")


def validate_account(account: Any) -> dict[str, Any]:
    if not isinstance(account, dict):
        fail("account must be an object")
    unknown = set(account) - EXPECTED_ACCOUNT_FIELDS
    if unknown:
        fail(f"unexpected account fields: {sorted(unknown)}")
    required = EXPECTED_ACCOUNT_FIELDS - {"quota_error"}
    missing = required - set(account)
    if missing:
        fail(f"missing account fields: {sorted(missing)}")
    if account["key_suffix"] != "redacted":
        fail("key_suffix must be the literal redacted")
    if account["quota_source"] not in ALLOWED_SOURCES:
        fail("quota_source is invalid")
    if account["health"] not in ALLOWED_HEALTH:
        fail("health is invalid")
    require_ratio(account["five_hour_utilization"], "five_hour_utilization")
    require_ratio(account["weekly_utilization"], "weekly_utilization")
    return account


def project(status: Any, observed_at: str) -> dict[str, Any]:
    if not isinstance(status, dict):
        fail("status must be an object")
    validate_no_secret_fields(status)
    if status.get("plugin") != "zai-coding-plan" or status.get("status") not in {"registered", "reconfigure_rejected"}:
        fail("unexpected plugin status")
    accounts = status.get("accounts")
    if not isinstance(accounts, list) or not accounts:
        fail("status must contain at least one account")
    projected_accounts = [validate_account(account) for account in accounts]
    return {
        "schemaVersion": 1,
        "observedAt": observed_at,
        "staleAfterSeconds": 300,
        "lane": "zai",
        "records": projected_accounts,
    }


def fetch_status(url: str, management_key: str, timeout: float) -> Any:
    request = urllib.request.Request(
        url,
        method="GET",
        headers={"Accept": "application/json", "Authorization": f"Bearer {management_key}"},
    )
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            if response.status != 200:
                fail(f"status endpoint returned HTTP {response.status}")
            raw = response.read(1_048_577)
    except urllib.error.HTTPError as error:
        fail(f"status endpoint returned HTTP {error.code}")
    except urllib.error.URLError:
        fail("status endpoint request failed")
    if len(raw) > 1_048_576:
        fail("status endpoint response exceeded 1 MiB")
    try:
        return json.loads(raw)
    except json.JSONDecodeError:
        fail("status endpoint returned invalid JSON")


def write_atomic(path: pathlib.Path, payload: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(f".{path.name}.tmp-{os.getpid()}")
    raw = (json.dumps(payload, separators=(",", ":"), sort_keys=True) + "\n").encode()
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o644)
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(raw)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
    finally:
        try:
            temporary.unlink()
        except FileNotFoundError:
            pass


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--url", default="http://127.0.0.1:8317/v0/management/plugins/zai-coding-plan/status")
    parser.add_argument("--output", default="/srv/cliproxy-usage/zai.json")
    parser.add_argument("--management-key-file", required=True)
    parser.add_argument("--timeout", type=float, default=5)
    args = parser.parse_args()
    management_key = pathlib.Path(args.management_key_file).read_text().strip()
    if not management_key:
        fail("management key file is empty")
    observed_at = datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")
    payload = project(fetch_status(args.url, management_key, args.timeout), observed_at)
    write_atomic(pathlib.Path(args.output), payload)
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except ValueError as error:
        print(f"collector-zai: {error}", file=sys.stderr)
        raise SystemExit(1)
