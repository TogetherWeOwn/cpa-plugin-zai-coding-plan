#!/usr/bin/env python3
"""Credential-free dogfood acceptance for paired-account scheduling and collector output."""
from __future__ import annotations

import json
import pathlib
import re
import subprocess
import sys
import tempfile

ROOT = pathlib.Path(__file__).resolve().parents[1]
FORBIDDEN_VALUES = ("fixture-plan-key", "release-integration-plan-key", "release-capability-plan-key")
FORBIDDEN_FIELDS = re.compile(r'"(?:api[-_]?key|authorization|credential|secret|token|key_hash|identity)"\s*:', re.I)


def run(*command: str) -> None:
    subprocess.run(command, cwd=ROOT, check=True)


def main() -> int:
    scheduler_pattern = "TestUsageFailureImpairsBothPairedCredentials|TestAuthSuspensionPrecedesExhaustionAndBothRecover|TestSchedulerHealthyDelegatesBuiltinRoundRobin|TestSchedulerDegradedExcludesSiblingsAndRoundRobinsHealthy|TestUsageCrossingQuotaThresholdImmediatelyBlocksScheduler"
    run("go", "test", "./src", "-run", scheduler_pattern, "-count=1")
    run("python3", "-m", "unittest", "discover", "-s", "deploy", "-p", "test_*.py")

    sys.path.insert(0, str(ROOT / "deploy"))
    import importlib.util
    spec = importlib.util.spec_from_file_location("collector_zai", ROOT / "deploy" / "collector-zai.py")
    collector = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(collector)
    fixture = json.loads((ROOT / "deploy/testdata/status-authoritative.json").read_text())
    projected = collector.project(fixture, "2026-09-10T15:00:00Z")
    with tempfile.TemporaryDirectory() as directory:
        output = pathlib.Path(directory) / "zai.json"
        collector.write_atomic(output, projected)
        raw = output.read_text()
        if any(value in raw for value in FORBIDDEN_VALUES) or FORBIDDEN_FIELDS.search(raw):
            raise SystemExit("projected collector output contains a forbidden secret marker")
        decoded = json.loads(raw)
        if decoded["lane"] != "zai" or decoded["records"][0]["key_suffix"] != "redacted":
            raise SystemExit("projected collector output failed lane/redaction checks")
    print("dogfood-local: paired 401/403/429 + threshold exclusion, healthy native round-robin, collector field contract, and redaction PASS")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
