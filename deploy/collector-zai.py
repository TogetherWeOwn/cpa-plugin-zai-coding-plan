#!/usr/bin/env python3
"""Project authenticated Z.ai plugin status into the sanitized collector lane."""
from __future__ import annotations

import argparse
import json
import os
import pathlib
import re
import stat
import sys
import urllib.error
import urllib.parse
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
SECRET_VALUE = re.compile(
    r"(?:bearer\s+\S+|(?:api[-_]?key|authorization|credential|secret|token|management[-_]?key|plan[-_]?key)\s*[:=]\s*\S+)",
    re.I,
)
ALLOWED_HEALTH = {"healthy", "exhausted", "suspended", "disabled", "config_error"}
ALLOWED_SOURCES = {"quota_api", "estimate"}
MAX_STRING_BYTES = 512
MAX_ACCOUNTS = 64
DEFAULT_ALLOWED_ORIGINS = ("http://127.0.0.1:8317", "http://[::1]:8317", "http://localhost:8317")


class NoRedirectHandler(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, file_pointer, code, message, headers, new_url):
        return None


def fail(message: str) -> NoReturn:
    raise ValueError(message)


def require_ratio(value: Any, field: str) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)) or not 0 <= float(value) <= 1:
        fail(f"{field} must be a ratio in [0,1]")
    return float(value)


def bound_string(value: Any, field: str, secret_markers: tuple[str, ...] = ()) -> str | None:
    if value is None:
        return None
    if not isinstance(value, str):
        fail(f"{field} must be a string or null")
    if len(value.encode()) > MAX_STRING_BYTES:
        fail(f"{field} exceeds {MAX_STRING_BYTES} bytes")
    if SECRET_VALUE.search(value) or any(marker and marker in value for marker in secret_markers):
        return "redacted"
    return value


def validate_no_secret_fields(value: Any, path: str = "status") -> None:
    if isinstance(value, dict):
        for key, child in value.items():
            if SECRET_FIELD.search(key) and key != "key_suffix":
                fail(f"secret-like field is forbidden: {path}.{key}")
            validate_no_secret_fields(child, f"{path}.{key}")
    elif isinstance(value, list):
        for index, child in enumerate(value):
            validate_no_secret_fields(child, f"{path}[{index}]")


def validate_account(account: Any, secret_markers: tuple[str, ...] = ()) -> dict[str, Any]:
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
    projected = dict(account)
    for key, value in projected.items():
        if isinstance(value, str) or value is None:
            projected[key] = bound_string(value, key, secret_markers)
    return projected


def project(status: Any, observed_at: str, secret_markers: tuple[str, ...] = ()) -> dict[str, Any]:
    if not isinstance(status, dict):
        fail("status must be an object")
    validate_no_secret_fields(status)
    if status.get("plugin") != "zai-coding-plan" or status.get("status") not in {"registered", "reconfigure_rejected"}:
        fail("unexpected plugin status")
    accounts = status.get("accounts")
    if not isinstance(accounts, list) or not accounts:
        fail("status must contain at least one account")
    if len(accounts) > MAX_ACCOUNTS:
        fail(f"status contains more than {MAX_ACCOUNTS} accounts")
    projected_accounts = [validate_account(account, secret_markers) for account in accounts]
    return {
        "schemaVersion": 1,
        "observedAt": bound_string(observed_at, "observedAt"),
        "staleAfterSeconds": 300,
        "lane": "zai",
        "records": projected_accounts,
    }


def canonical_origin(url: str) -> str:
    parsed = urllib.parse.urlsplit(url)
    if parsed.scheme not in {"http", "https"} or not parsed.hostname or parsed.username or parsed.password:
        fail("management URL must be an absolute HTTP(S) URL without userinfo")
    try:
        port = parsed.port
    except ValueError:
        fail("management URL has an invalid port")
    default_port = 80 if parsed.scheme == "http" else 443
    host = parsed.hostname.lower()
    rendered_host = f"[{host}]" if ":" in host else host
    return f"{parsed.scheme}://{rendered_host}:{port or default_port}"


def validate_management_url(url: str, allowed_origins: tuple[str, ...]) -> None:
    parsed = urllib.parse.urlsplit(url)
    if parsed.query or parsed.fragment:
        fail("management URL must not contain a query or fragment")
    if parsed.path != "/v0/management/plugins/zai-coding-plan/status":
        fail("management URL path is not approved")
    origin = canonical_origin(url)
    allowed = {canonical_origin(value) for value in allowed_origins}
    if origin not in allowed:
        fail("management URL origin is not approved")


def fetch_status(url: str, management_key: str, timeout: float, allowed_origins: tuple[str, ...] = DEFAULT_ALLOWED_ORIGINS) -> Any:
    validate_management_url(url, allowed_origins)
    request = urllib.request.Request(
        url,
        method="GET",
        headers={"Accept": "application/json", "Authorization": f"Bearer {management_key}"},
    )
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirectHandler())
    try:
        with opener.open(request, timeout=timeout) as response:
            if response.status != 200:
                fail(f"status endpoint returned HTTP {response.status}")
            raw = response.read(1_048_577)
    except urllib.error.HTTPError as error:
        if 300 <= error.code < 400:
            fail("status endpoint redirect rejected")
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
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    os.chmod(path.parent, 0o700)
    temporary = path.with_name(f".{path.name}.tmp-{os.getpid()}")
    raw = (json.dumps(payload, separators=(",", ":"), sort_keys=True) + "\n").encode()
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    try:
        os.fchmod(fd, stat.S_IRUSR | stat.S_IWUSR)
        with os.fdopen(fd, "wb") as handle:
            handle.write(raw)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
        os.chmod(path, 0o600)
        directory_fd = os.open(path.parent, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    finally:
        try:
            temporary.unlink()
        except FileNotFoundError:
            pass


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--url", default="http://127.0.0.1:8317/v0/management/plugins/zai-coding-plan/status")
    parser.add_argument("--allowed-origin", action="append", default=[])
    parser.add_argument("--output", default="/srv/cliproxy-usage/zai.json")
    parser.add_argument("--management-key-file", required=True)
    parser.add_argument("--secret-marker-file", action="append", default=[])
    parser.add_argument("--timeout", type=float, default=5)
    args = parser.parse_args()
    management_key = pathlib.Path(args.management_key_file).read_text().strip()
    if not management_key:
        fail("management key file is empty")
    secret_markers = tuple(
        marker
        for marker in (pathlib.Path(path).read_text().strip() for path in args.secret_marker_file)
        if marker
    )
    allowed_origins = DEFAULT_ALLOWED_ORIGINS + tuple(args.allowed_origin)
    observed_at = datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")
    payload = project(fetch_status(args.url, management_key, args.timeout, allowed_origins), observed_at, secret_markers)
    write_atomic(pathlib.Path(args.output), payload)
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except ValueError as error:
        print(f"collector-zai: {error}", file=sys.stderr)
        raise SystemExit(1)
