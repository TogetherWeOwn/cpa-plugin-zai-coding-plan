#!/usr/bin/env python3
"""Project authenticated OpenCode Go plugin status into the sanitized collector lane."""
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

COORDINATOR_REQUIRED_FIELDS = {"plugin", "status", "version", "generated_at", "providers"}
COORDINATOR_EXPECTED_FIELDS = COORDINATOR_REQUIRED_FIELDS | {"validation_error"}
ALLOWED_COORDINATOR_STATUS = {"registered", "reconfigure_rejected"}
PROVIDER_ID = "opencode-go"
PROVIDER_REQUIRED_FIELDS = {"provider", "status", "credential_bound", "observation_gaps"}
PROVIDER_EXPECTED_FIELDS = PROVIDER_REQUIRED_FIELDS | {"validation_error", "accounts"}
ALLOWED_PROVIDER_STATUS = {"registered", "reconfigure_rejected"}
ACCOUNT_REQUIRED_FIELDS = {"name", "windows"}
ACCOUNT_EXPECTED_FIELDS = ACCOUNT_REQUIRED_FIELDS | {"disabled"}
WINDOW_KINDS = {"five_hour", "weekly", "monthly"}
WINDOW_REQUIRED_FIELDS = {"known", "exhausted"}
WINDOW_EXPECTED_FIELDS = WINDOW_REQUIRED_FIELDS | {"utilization", "resets_at", "source", "authoritative"}
SECRET_FIELD = re.compile(r"(?:api[-_]?key|authorization|credential|secret|token|key_hash|identity)", re.I)
SECRET_FIELD_EXCEPTIONS = {"key_suffix", "credential_bound"}
SECRET_VALUE = re.compile(
    r"(?:bearer\s+\S+|(?:api[-_]?key|authorization|credential|secret|token|management[-_]?key|dashboard[-_]?api[-_]?key)\s*[:=]\s*\S+)",
    re.I,
)
RFC3339 = re.compile(r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$")
MAX_STRING_BYTES = 512
MAX_ACCOUNTS = 64
MAX_OBSERVATION_GAPS = 16
DEFAULT_ALLOWED_ORIGINS = ("http://127.0.0.1:8317", "http://[::1]:8317", "http://localhost:8317")
MANAGEMENT_STATUS_PATH = "/v0/management/plugins/subscription-pool/status"
TRUSTED_OUTPUT = pathlib.Path("/srv/cliproxy-usage/opencode-go.json")


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
            if SECRET_FIELD.search(key) and key not in SECRET_FIELD_EXCEPTIONS:
                fail(f"secret-like field is forbidden: {path}.{key}")
            validate_no_secret_fields(child, f"{path}.{key}")
    elif isinstance(value, list):
        for index, child in enumerate(value):
            validate_no_secret_fields(child, f"{path}[{index}]")


def validate_window(kind: str, window: Any, secret_markers: tuple[str, ...]) -> dict[str, Any]:
    if not isinstance(window, dict):
        fail(f"window {kind} must be an object")
    unknown = set(window) - WINDOW_EXPECTED_FIELDS
    if unknown:
        fail(f"unexpected window fields for {kind}: {sorted(unknown)}")
    missing = WINDOW_REQUIRED_FIELDS - set(window)
    if missing:
        fail(f"missing window fields for {kind}: {sorted(missing)}")
    if not isinstance(window["known"], bool):
        fail(f"{kind}.known must be a boolean")
    if not isinstance(window["exhausted"], bool):
        fail(f"{kind}.exhausted must be a boolean")
    known = window["known"]
    projected: dict[str, Any] = {"known": known, "exhausted": window["exhausted"]}
    if "utilization" in window:
        if not known:
            fail(f"{kind} must not report utilization while known is false")
        projected["utilization"] = require_ratio(window["utilization"], f"{kind}.utilization")
    if "resets_at" in window:
        if not known:
            fail(f"{kind} must not report resets_at while known is false")
        projected["resets_at"] = require_timestamp(window["resets_at"], f"{kind}.resets_at")
    if "source" in window:
        projected["source"] = bound_string(window["source"], f"{kind}.source", secret_markers)
    if "authoritative" in window:
        if not isinstance(window["authoritative"], bool):
            fail(f"{kind}.authoritative must be a boolean")
        projected["authoritative"] = window["authoritative"]
    return projected


def validate_account(account: Any, secret_markers: tuple[str, ...]) -> dict[str, Any]:
    if not isinstance(account, dict):
        fail("account must be an object")
    unknown = set(account) - ACCOUNT_EXPECTED_FIELDS
    if unknown:
        fail(f"unexpected account fields: {sorted(unknown)}")
    missing = ACCOUNT_REQUIRED_FIELDS - set(account)
    if missing:
        fail(f"missing account fields: {sorted(missing)}")
    name = bound_string(account["name"], "name", secret_markers)
    disabled = account.get("disabled", False)
    if not isinstance(disabled, bool):
        fail("disabled must be a boolean")
    windows = account["windows"]
    if not isinstance(windows, dict) or set(windows) != WINDOW_KINDS:
        fail("windows must contain exactly five_hour, weekly, and monthly")
    projected_windows = {kind: validate_window(kind, windows[kind], secret_markers) for kind in sorted(WINDOW_KINDS)}
    return {"name": name, "disabled": disabled, "windows": projected_windows}


def project_provider(provider: Any, secret_markers: tuple[str, ...]) -> dict[str, Any]:
    if not isinstance(provider, dict):
        fail("provider status must be an object")
    unknown = set(provider) - PROVIDER_EXPECTED_FIELDS
    if unknown:
        fail(f"unexpected provider status fields: {sorted(unknown)}")
    missing = PROVIDER_REQUIRED_FIELDS - set(provider)
    if missing:
        fail(f"missing provider status fields: {sorted(missing)}")
    if provider["provider"] != PROVIDER_ID:
        fail("unexpected provider identity")
    if provider["status"] not in ALLOWED_PROVIDER_STATUS:
        fail("unexpected provider status")
    if not isinstance(provider["credential_bound"], bool):
        fail("credential_bound must be a boolean")
    gaps = provider["observation_gaps"]
    if not isinstance(gaps, list) or not all(isinstance(gap, str) for gap in gaps):
        fail("observation_gaps must be a list of strings")
    if len(gaps) > MAX_OBSERVATION_GAPS:
        fail(f"observation_gaps contains more than {MAX_OBSERVATION_GAPS} entries")
    projected_gaps = [bound_string(gap, "observation_gaps[]", secret_markers) for gap in gaps]
    result: dict[str, Any] = {
        "status": provider["status"],
        "credential_bound": provider["credential_bound"],
        "observation_gaps": projected_gaps,
    }
    if "validation_error" in provider:
        result["validation_error"] = bound_string(provider["validation_error"], "validation_error", secret_markers, nullable=True)
    accounts = provider.get("accounts", [])
    if not isinstance(accounts, list):
        fail("accounts must be a list")
    if len(accounts) > MAX_ACCOUNTS:
        fail(f"provider contains more than {MAX_ACCOUNTS} accounts")
    result["records"] = [validate_account(account, secret_markers) for account in accounts]
    if provider["status"] == "reconfigure_rejected":
        for record in result["records"]:
            for kind in WINDOW_KINDS:
                record["windows"][kind] = {"known": False, "exhausted": False, "source": "unknown"}
    return result


def project(status: Any, observed_at: str, secret_markers: tuple[str, ...] = ()) -> dict[str, Any]:
    if not isinstance(status, dict):
        fail("status must be an object")
    unknown = set(status) - COORDINATOR_EXPECTED_FIELDS
    if unknown:
        fail(f"unexpected status fields: {sorted(unknown)}")
    missing = COORDINATOR_REQUIRED_FIELDS - set(status)
    if missing:
        fail(f"missing status fields: {sorted(missing)}")
    validate_no_secret_fields(status)
    if status["plugin"] != "subscription-pool" or status["status"] not in ALLOWED_COORDINATOR_STATUS:
        fail("unexpected coordinator plugin status")
    bound_string(status["version"], "version")
    require_timestamp(status["generated_at"], "generated_at")
    if "validation_error" in status:
        bound_string(status["validation_error"], "validation_error", secret_markers, nullable=True)
    providers = status["providers"]
    if not isinstance(providers, dict):
        fail("providers must be an object")
    if PROVIDER_ID not in providers:
        fail(f"providers is missing the {PROVIDER_ID} entry")
    projected_provider = project_provider(providers[PROVIDER_ID], secret_markers)
    return {
        "schemaVersion": 1,
        "observedAt": require_timestamp(observed_at, "observedAt"),
        "staleAfterSeconds": 300,
        "lane": PROVIDER_ID,
        **projected_provider,
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
    if parsed.path != MANAGEMENT_STATUS_PATH:
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
    parser.add_argument("--url", default="http://127.0.0.1:8317" + MANAGEMENT_STATUS_PATH)
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
        print(f"collector-opencodego: {error}", file=sys.stderr)
        raise SystemExit(1)
    except OSError:
        print("collector-opencodego: filesystem operation failed", file=sys.stderr)
        raise SystemExit(1)
