#!/usr/bin/env python3
"""Classify a bounded plugin DELETE result without disclosing its response body."""
from __future__ import annotations

import json
import pathlib
import sys


def main() -> int:
    if len(sys.argv) != 4:
        raise ValueError("usage: rollback-delete-policy <curl-exit> <http-status> <response-file>")
    try:
        curl_exit = int(sys.argv[1])
        http_status = int(sys.argv[2])
    except ValueError:
        raise ValueError("curl exit and HTTP status must be integers")
    response_path = pathlib.Path(sys.argv[3])
    if curl_exit != 0:
        print(f"rollback-delete: curl exit {curl_exit}; continuing rollback", file=sys.stderr)
        return 0
    if 200 <= http_status < 300:
        return 0
    try:
        raw = response_path.read_bytes()
        if len(raw) > 1_048_576:
            raise ValueError
        response = json.loads(raw) if raw else {}
    except (OSError, UnicodeError, json.JSONDecodeError, ValueError):
        raise ValueError(f"plugin removal returned an unexpected {http_status} response")
    if http_status == 404 and isinstance(response, dict) and response.get("error") == "plugin_not_found":
        return 0
    if http_status == 409 and isinstance(response, dict) and response.get("error") == "plugin_delete_requires_restart":
        return 10
    if http_status == 404:
        raise ValueError("plugin removal returned an unexpected 404 response")
    print(f"rollback-delete: HTTP {http_status}; continuing rollback", file=sys.stderr)
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except ValueError as error:
        print(f"rollback-delete: {error}", file=sys.stderr)
        raise SystemExit(1)
