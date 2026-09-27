#!/usr/bin/env python3
"""Project authenticated Z.ai plugin status into the sanitized collector lane."""
from __future__ import annotations

import argparse
import json
import math
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

REQUIRED_STATUS_FIELDS = {"plugin", "status", "version", "generated_at", "accounts"}
EXPECTED_STATUS_FIELDS = REQUIRED_STATUS_FIELDS | {"validation_error"}
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
RFC3339 = re.compile(r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$")
ALLOWED_HEALTH = {"healthy", "exhausted", "suspended", "disabled", "config_error"}
ALLOWED_SOURCES = {"quota_api", "estimate"}
STRING_FIELDS = {
    "name",
    "key_suffix",
    "plan",
    "five_hour_resets_at",
    "weekly_resets_at",
    "quota_source",
    "quota_observed_at",
    "quota_error",
    "health",
    "estimator_complete_since",
    "dedup_mode",
}
NULLABLE_STRING_FIELDS = {"five_hour_resets_at", "weekly_resets_at", "quota_error", "estimator_complete_since"}
TIMESTAMP_FIELDS = {"five_hour_resets_at", "weekly_resets_at", "quota_observed_at", "estimator_complete_since"}
BOOLEAN_FIELDS = {
    "quota_stale",
    "offpeak",
    "delivery_warning",
    "persistence_warning",
    "unknown_model_warning",
    "heuristic_dedup_warning",
}
RATIO_FIELDS = {"five_hour_utilization", "weekly_utilization"}
MAX_STRING_BYTES = 512
MAX_ACCOUNTS = 64
DEFAULT_ALLOWED_ORIGINS = ("http://127.0.0.1:8317", "http://[::1]:8317", "http://localhost:8317")
TRUSTED_OUTPUT = pathlib.Path("/srv/cliproxy-usage/zai.json")


class NoRedirectHandler(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, file_pointer, code, message, headers, new_url):
        return None


def fail(message: str) -> NoReturn:
    raise ValueError(message)


def require_ratio(value: Any, field: str) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        fail(f"{field} must be a finite ratio in [0,1]")
    projected = float(value)
    if not math.isfinite(projected) or not 0 <= projected <= 1:
        fail(f"{field} must be a finite ratio in [0,1]")
    return projected


def require_nonnegative_integer(value: Any, field: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int) or value < 0:
        fail(f"{field} must be a non-negative integer")
    return value


def bound_string(value: Any, field: str, secret_markers: tuple[str, ...] = (), nullable: bool = False) -> str | None:
    if value is None:
        if nullable:
            return None
        fail(f"{field} must be a non-empty string")
    if not isinstance(value, str):
        fail(f"{field} must be a string" + (" or null" if nullable else ""))
    if not value and not nullable:
        fail(f"{field} must be a non-empty string")
    if len(value.encode()) > MAX_STRING_BYTES:
        fail(f"{field} exceeds {MAX_STRING_BYTES} bytes")
    if SECRET_VALUE.search(value) or any(marker and marker in value for marker in secret_markers):
        return "redacted"
    return value


def require_timestamp(value: Any, field: str, nullable: bool = False) -> str | None:
    projected = bound_string(value, field, nullable=nullable)
    if projected is None:
        return None
    if not RFC3339.fullmatch(projected):
        fail(f"{field} must be an RFC 3339 timestamp")
    try:
        parsed = datetime.fromisoformat(projected[:-1] + "+00:00" if projected.endswith("Z") else projected)
    except ValueError:
        fail(f"{field} must be an RFC 3339 timestamp")
    if parsed.tzinfo is None:
        fail(f"{field} must be an RFC 3339 timestamp")
    return projected


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
    projected: dict[str, Any] = {}
    for key in STRING_FIELDS:
        if key == "quota_error" and key not in account:
            continue
        if key in TIMESTAMP_FIELDS:
            projected[key] = require_timestamp(account[key], key, key in NULLABLE_STRING_FIELDS)
        else:
            projected[key] = bound_string(account[key], key, secret_markers, key in NULLABLE_STRING_FIELDS)
    for key in BOOLEAN_FIELDS:
        if not isinstance(account[key], bool):
            fail(f"{key} must be a boolean")
        projected[key] = account[key]
    for key in RATIO_FIELDS:
        projected[key] = require_ratio(account[key], key)
    projected["quota_age_seconds"] = require_nonnegative_integer(account["quota_age_seconds"], "quota_age_seconds")
    return projected


def project(status: Any, observed_at: str, secret_markers: tuple[str, ...] = ()) -> dict[str, Any]:
    if not isinstance(status, dict):
        fail("status must be an object")
    unknown = set(status) - EXPECTED_STATUS_FIELDS
    if unknown:
        fail(f"unexpected status fields: {sorted(unknown)}")
    missing = REQUIRED_STATUS_FIELDS - set(status)
    if missing:
        fail(f"missing status fields: {sorted(missing)}")
    validate_no_secret_fields(status)
    if status["plugin"] != "zai-coding-plan" or status["status"] not in {"registered", "reconfigure_rejected"}:
        fail("unexpected plugin status")
    bound_string(status["version"], "version")
    require_timestamp(status["generated_at"], "generated_at")
    if "validation_error" in status:
        bound_string(status["validation_error"], "validation_error", secret_markers, nullable=True)
    accounts = status["accounts"]
    if not isinstance(accounts, list) or not accounts:
        fail("status must contain at least one account")
    if len(accounts) > MAX_ACCOUNTS:
        fail(f"status contains more than {MAX_ACCOUNTS} accounts")
    projected_accounts = [validate_account(account, secret_markers) for account in accounts]
    if status["status"] == "reconfigure_rejected":
        for account in projected_accounts:
            account["health"] = "config_error"
            account["quota_stale"] = True
    return {
        "schemaVersion": 1,
        "observedAt": require_timestamp(observed_at, "observedAt"),
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


def load_json_strict(raw: bytes) -> Any:
    try:
        return json.loads(raw, parse_constant=lambda _value: fail("status endpoint returned non-RFC JSON"))
    except UnicodeDecodeError:
        fail("status endpoint returned invalid UTF-8 JSON")
    except json.JSONDecodeError:
        fail("status endpoint returned invalid JSON")


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
    except (urllib.error.URLError, TimeoutError, OSError, ValueError):
        fail("status endpoint request failed")
    if len(raw) > 1_048_576:
        fail("status endpoint response exceeded 1 MiB")
    return load_json_strict(raw)


def read_single_line_secret(path: pathlib.Path, label: str) -> str:
    try:
        raw = path.read_bytes()
    except OSError:
        fail(f"{label} file could not be read")
    lines = raw.splitlines()
    if len(lines) != 1 or not lines[0] or lines[0].strip() != lines[0]:
        fail(f"{label} file must contain exactly one non-empty line")
    try:
        return lines[0].decode("utf-8")
    except UnicodeDecodeError:
        fail(f"{label} file must contain valid UTF-8")


def open_trusted_output_directory(
    path: pathlib.Path,
    expected_owner_uid: int,
    trusted_output: pathlib.Path,
    filesystem_root: pathlib.Path,
) -> int:
    if not path.is_absolute() or path != trusted_output or trusted_output != TRUSTED_OUTPUT:
        fail(f"output path must be exactly {TRUSTED_OUTPUT}")
    if not filesystem_root.is_absolute():
        fail("filesystem root must be absolute")
    parts = path.parts[1:-1]
    flags = os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0)
    try:
        directory_fd = os.open(filesystem_root, flags)
    except OSError:
        fail("filesystem root could not be opened securely")
    try:
        for component in parts:
            try:
                next_fd = os.open(component, flags, dir_fd=directory_fd)
            except OSError:
                fail("trusted output directory path could not be opened securely")
            os.close(directory_fd)
            directory_fd = next_fd
        opened_stat = os.fstat(directory_fd)
        if not stat.S_ISDIR(opened_stat.st_mode):
            fail("trusted output directory must be a real directory")
        if opened_stat.st_uid != expected_owner_uid:
            fail("trusted output directory has the wrong owner")
        if stat.S_IMODE(opened_stat.st_mode) != 0o700:
            fail("trusted output directory must have mode 0700")
        return directory_fd
    except Exception:
        os.close(directory_fd)
        raise


def write_atomic(
    path: pathlib.Path,
    payload: dict[str, Any],
    expected_owner_uid: int = 0,
    *,
    trusted_output: pathlib.Path = TRUSTED_OUTPUT,
    filesystem_root: pathlib.Path = pathlib.Path("/"),
) -> None:
    directory_fd = open_trusted_output_directory(path, expected_owner_uid, trusted_output, filesystem_root)
    temporary_name = f".{path.name}.tmp-{os.getpid()}"
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0)
    try:
        try:
            raw = (json.dumps(payload, allow_nan=False, separators=(",", ":"), sort_keys=True) + "\n").encode()
        except (TypeError, ValueError):
            fail("collector payload is not strict JSON")
        try:
            target_stat = os.stat(path.name, dir_fd=directory_fd, follow_symlinks=False)
        except FileNotFoundError:
            target_stat = None
        except OSError:
            fail("trusted output file could not be inspected securely")
        if target_stat is not None and stat.S_ISLNK(target_stat.st_mode):
            fail("trusted output file must not be a symlink")
        try:
            fd = os.open(temporary_name, flags, 0o600, dir_fd=directory_fd)
        except OSError:
            fail("temporary output file could not be created securely")
        try:
            with os.fdopen(fd, "wb") as handle:
                handle.write(raw)
                handle.flush()
                os.fsync(handle.fileno())
        except Exception:
            try:
                os.close(fd)
            except OSError:
                pass
            raise
        os.replace(temporary_name, path.name, src_dir_fd=directory_fd, dst_dir_fd=directory_fd)
        os.fsync(directory_fd)
    finally:
        try:
            os.unlink(temporary_name, dir_fd=directory_fd)
        except FileNotFoundError:
            pass
        finally:
            os.close(directory_fd)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--url", default="http://127.0.0.1:8317/v0/management/plugins/zai-coding-plan/status")
    parser.add_argument("--allowed-origin", action="append", default=[])
    parser.add_argument("--management-key-file", required=True)
    parser.add_argument("--secret-marker-file", action="append", default=[])
    parser.add_argument("--timeout", type=float, default=5)
    args = parser.parse_args()
    management_key = read_single_line_secret(pathlib.Path(args.management_key_file), "management key")
    secret_markers = tuple(
        read_single_line_secret(pathlib.Path(path), "secret marker") for path in args.secret_marker_file
    )
    allowed_origins = DEFAULT_ALLOWED_ORIGINS + tuple(args.allowed_origin)
    observed_at = datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")
    payload = project(fetch_status(args.url, management_key, args.timeout, allowed_origins), observed_at, secret_markers)
    write_atomic(TRUSTED_OUTPUT, payload)
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except ValueError as error:
        print(f"collector-zai: {error}", file=sys.stderr)
        raise SystemExit(1)
    except OSError:
        print("collector-zai: filesystem operation failed", file=sys.stderr)
        raise SystemExit(1)
