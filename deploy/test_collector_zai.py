import http.server
import importlib.util
import json
import math
import os
import pathlib
import stat
import tempfile
import threading
import time
import unittest
from unittest import mock

MODULE_PATH = pathlib.Path(__file__).with_name("collector-zai.py")
SPEC = importlib.util.spec_from_file_location("collector_zai", MODULE_PATH)
collector = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(collector)
FIXTURES = pathlib.Path(__file__).with_name("testdata")


class CollectorZaiTest(unittest.TestCase):
    def fixture(self, name):
        return json.loads((FIXTURES / name).read_text())

    def test_projects_required_field_names(self):
        out = collector.project(self.fixture("status-authoritative.json"), "2026-09-10T15:00:00Z")
        self.assertEqual(out["lane"], "zai")
        self.assertEqual(out["records"][0]["quota_source"], "quota_api")
        self.assertEqual(set(out["records"][0]), collector.EXPECTED_ACCOUNT_FIELDS - {"quota_error"})

    def test_reconfigure_rejected_marks_all_capacity_unavailable(self):
        value = self.fixture("status-authoritative.json")
        value["status"] = "reconfigure_rejected"
        value["validation_error"] = "invalid replacement configuration"
        out = collector.project(value, "2026-09-10T15:00:00Z")
        self.assertTrue(out["records"][0]["quota_stale"])
        self.assertEqual(out["records"][0]["health"], "config_error")

    def test_bounds_and_redacts_every_persisted_string(self):
        value = self.fixture("status-fallback.json")
        value["accounts"][0]["quota_error"] = "management-key=fixture-management-marker"
        out = collector.project(value, "2026-09-10T15:00:00Z")
        self.assertEqual(out["records"][0]["quota_error"], "redacted")
        value["accounts"][0]["quota_error"] = "bare-fixture-plan-marker"
        out = collector.project(value, "2026-09-10T15:00:00Z", ("bare-fixture-plan-marker",))
        self.assertEqual(out["records"][0]["quota_error"], "redacted")
        for field in ("name", "plan", "five_hour_resets_at", "quota_observed_at", "dedup_mode"):
            with self.subTest(field=field):
                value = self.fixture("status-authoritative.json")
                value["accounts"][0][field] = "x" * (collector.MAX_STRING_BYTES + 1)
                with self.assertRaises(ValueError):
                    collector.project(value, "2026-09-10T15:00:00Z")

    def test_rejects_null_names_fractional_age_and_nonfinite_numbers(self):
        cases = (
            ("name", None),
            ("quota_age_seconds", 1.5),
            ("quota_age_seconds", math.inf),
            ("five_hour_utilization", math.nan),
            ("weekly_utilization", math.inf),
        )
        for field, value in cases:
            status = self.fixture("status-authoritative.json")
            status["accounts"][0][field] = value
            with self.subTest(field=field, value=value), self.assertRaises(ValueError):
                collector.project(status, "2026-09-10T15:00:00Z")

    def test_rejects_invalid_timestamps(self):
        for field in ("generated_at", "observedAt", "quota_observed_at", "five_hour_resets_at", "estimator_complete_since"):
            with self.subTest(field=field):
                status = self.fixture("status-authoritative.json")
                observed_at = "2026-09-10T15:00:00Z"
                if field == "generated_at":
                    status[field] = "not-a-timestamp"
                elif field == "observedAt":
                    observed_at = "not-a-timestamp"
                else:
                    status["accounts"][0][field] = "not-a-timestamp"
                with self.assertRaises(ValueError):
                    collector.project(status, observed_at)

    def test_rejects_unknown_top_level_fields_and_non_rfc_json(self):
        status = self.fixture("status-authoritative.json")
        status["extra"] = "value"
        with self.assertRaises(ValueError):
            collector.project(status, "2026-09-10T15:00:00Z")
        for constant in (b"NaN", b"Infinity", b"-Infinity"):
            raw = b'{"value":' + constant + b"}"
            with self.subTest(constant=constant), self.assertRaisesRegex(ValueError, "non-RFC JSON"):
                collector.load_json_strict(raw)

    def test_rejects_non_string_and_nested_values_in_allowed_fields(self):
        cases = (
            ("quota_error", 123),
            ("name", 123),
            ("delivery_warning", {"message": "fixture-plan-marker"}),
            ("quota_stale", "false"),
            ("quota_age_seconds", {"value": 1}),
        )
        for field, value in cases:
            status = self.fixture("status-fallback.json")
            status["accounts"][0][field] = value
            with self.subTest(field=field), self.assertRaises(ValueError):
                collector.project(status, "2026-09-10T15:00:00Z", ("fixture-plan-marker",))

    def test_rejects_secret_like_fields_and_nonredacted_suffix(self):
        for field in ("api_key", "authorization", "credential", "key_hash", "identity"):
            value = self.fixture("status-authoritative.json")
            value["accounts"][0][field] = "fixture-secret"
            with self.subTest(field=field), self.assertRaises(ValueError):
                collector.project(value, "2026-09-10T15:00:00Z")
        value = self.fixture("status-authoritative.json")
        value["accounts"][0]["key_suffix"] = "last-four"
        with self.assertRaises(ValueError):
            collector.project(value, "2026-09-10T15:00:00Z")

    def test_reads_only_one_nonempty_secret_line_without_leaking_content(self):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / "key"
            for raw in (b"", b"first\nsecond\n", b" padded \n"):
                path.write_bytes(raw)
                with self.subTest(raw=raw), self.assertRaises(ValueError) as caught:
                    collector.read_single_line_secret(path, "management key")
                for line in raw.decode(errors="ignore").splitlines():
                    if line:
                        self.assertNotIn(line, str(caught.exception))
            path.write_text("fixture-key\n")
            self.assertEqual(collector.read_single_line_secret(path, "management key"), "fixture-key")

    def test_rejects_unapproved_management_origins(self):
        with self.assertRaises(ValueError):
            collector.validate_management_url("https://example.invalid/status", collector.DEFAULT_ALLOWED_ORIGINS)
        collector.validate_management_url(
            "https://management.internal/v0/management/plugins/zai-coding-plan/status",
            ("https://management.internal",),
        )
        for suffix in ("?next=1", "#fragment"):
            with self.subTest(suffix=suffix), self.assertRaises(ValueError):
                collector.validate_management_url(
                    "http://127.0.0.1:8317/v0/management/plugins/zai-coding-plan/status" + suffix,
                    collector.DEFAULT_ALLOWED_ORIGINS,
                )

    def test_rejects_redirect_without_forwarding_authorization(self):
        seen = []

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                seen.append((self.path, self.headers.get("Authorization")))
                if self.path == "/v0/management/plugins/zai-coding-plan/status":
                    self.send_response(302)
                    self.send_header("Location", f"http://127.0.0.1:{self.server.server_port}/sink")
                    self.end_headers()
                else:
                    self.send_response(200)
                    self.end_headers()

            def log_message(self, *_args):
                pass

        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        origin = f"http://127.0.0.1:{server.server_port}"
        try:
            with self.assertRaisesRegex(ValueError, "redirect rejected"):
                collector.fetch_status(
                    f"{origin}/v0/management/plugins/zai-coding-plan/status",
                    "fixture-management-marker",
                    2,
                    (origin,),
                )
        finally:
            server.shutdown()
            thread.join()
            server.server_close()
        self.assertEqual(
            seen,
            [("/v0/management/plugins/zai-coding-plan/status", "Bearer fixture-management-marker")],
        )

    def test_rejects_nonpositive_or_nonfinite_request_deadlines(self):
        origin = "http://127.0.0.1:1"
        url = origin + "/v0/management/plugins/zai-coding-plan/status"
        for timeout in (0, -1, math.nan, math.inf, -math.inf):
            with self.subTest(timeout=timeout), self.assertRaisesRegex(ValueError, "positive finite"):
                collector.fetch_status(url, "fixture-management-marker", timeout, (origin,))

    def test_rejects_preexisting_process_alarm(self):
        origin = "http://127.0.0.1:1"
        url = origin + "/v0/management/plugins/zai-coding-plan/status"
        with mock.patch.object(collector.signal, "getsignal", return_value=lambda *_args: None):
            with self.assertRaisesRegex(ValueError, "unused process alarm"):
                collector.fetch_status(url, "fixture-management-marker", 1, (origin,))
        with mock.patch.object(collector.signal, "getitimer", return_value=(1.0, 0.0)):
            with self.assertRaisesRegex(ValueError, "unused process alarm"):
                collector.fetch_status(url, "fixture-management-marker", 1, (origin,))

    def test_drip_response_exceeds_wall_clock_deadline(self):
        payload = json.dumps(self.fixture("status-authoritative.json")).encode()

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(200)
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                for byte in payload:
                    try:
                        self.wfile.write(bytes((byte,)))
                        self.wfile.flush()
                    except BrokenPipeError:
                        return
                    time.sleep(0.02)

            def log_message(self, *_args):
                pass

        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        origin = f"http://127.0.0.1:{server.server_port}"
        started = time.monotonic()
        try:
            with self.assertRaisesRegex(ValueError, "wall-clock deadline"):
                collector.fetch_status(
                    f"{origin}/v0/management/plugins/zai-coding-plan/status",
                    "fixture-management-marker",
                    0.15,
                    (origin,),
                )
        finally:
            server.shutdown()
            server.server_close()
        elapsed = time.monotonic() - started
        thread.join(timeout=1)
        self.assertLess(elapsed, 1.0)

    def trusted_output(self, root):
        parent = root / "srv" / "cliproxy-usage"
        parent.mkdir(parents=True)
        parent.chmod(0o700)
        return pathlib.Path("/srv/cliproxy-usage/zai.json"), parent

    def write_trusted(self, root, path, payload, expected_owner_uid=None):
        collector.write_atomic(
            path,
            payload,
            expected_owner_uid=os.getuid() if expected_owner_uid is None else expected_owner_uid,
            trusted_output=pathlib.Path("/srv/cliproxy-usage/zai.json"),
            filesystem_root=root,
        )

    def test_atomic_write_enforces_initial_and_replacement_file_mode(self):
        payload = collector.project(self.fixture("status-authoritative.json"), "2026-09-10T15:00:00Z")
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            path, parent = self.trusted_output(root)
            self.write_trusted(root, path, payload)
            output = parent / "zai.json"
            self.assertEqual(stat.S_IMODE(parent.stat().st_mode), 0o700)
            self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o600)
            os.chmod(output, 0o644)
            self.write_trusted(root, path, payload)
            self.assertEqual(stat.S_IMODE(parent.stat().st_mode), 0o700)
            self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o600)

    def test_atomic_write_rejects_wrong_root_path_without_chmod(self):
        payload = collector.project(self.fixture("status-authoritative.json"), "2026-09-10T15:00:00Z")
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            path, parent = self.trusted_output(root)
            parent.chmod(0o755)
            wrong = pathlib.Path("/srv/caller-selected/zai.json")
            with self.assertRaisesRegex(ValueError, "exactly"):
                self.write_trusted(root, wrong, payload)
            self.assertEqual(stat.S_IMODE(parent.stat().st_mode), 0o755)
            self.assertFalse((root / wrong.relative_to("/")).exists())

    def test_atomic_write_rejects_symlinked_ancestor_component(self):
        payload = collector.project(self.fixture("status-authoritative.json"), "2026-09-10T15:00:00Z")
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            victim = root / "victim"
            victim.mkdir()
            victim.chmod(0o700)
            (root / "srv").symlink_to(victim, target_is_directory=True)
            path = pathlib.Path("/srv/cliproxy-usage/zai.json")
            with self.assertRaisesRegex(ValueError, "could not be opened securely"):
                self.write_trusted(root, path, payload)
            self.assertFalse((victim / "cliproxy-usage" / "zai.json").exists())

    def test_atomic_write_rejects_wrong_owner_or_mode_without_chmod(self):
        payload = collector.project(self.fixture("status-authoritative.json"), "2026-09-10T15:00:00Z")
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            path, parent = self.trusted_output(root)
            with self.assertRaisesRegex(ValueError, "wrong owner"):
                self.write_trusted(root, path, payload, expected_owner_uid=os.getuid() + 1)
            parent.chmod(0o755)
            with self.assertRaisesRegex(ValueError, "mode 0700"):
                self.write_trusted(root, path, payload)
            self.assertEqual(stat.S_IMODE(parent.stat().st_mode), 0o755)

    def test_atomic_write_rejects_symlinked_output_component(self):
        payload = collector.project(self.fixture("status-authoritative.json"), "2026-09-10T15:00:00Z")
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            path, parent = self.trusted_output(root)
            victim = root / "victim.json"
            victim.write_text("unchanged")
            (parent / "zai.json").symlink_to(victim)
            with self.assertRaisesRegex(ValueError, "must not be a symlink"):
                self.write_trusted(root, path, payload)
            self.assertEqual(victim.read_text(), "unchanged")

    def test_atomic_write_fsyncs_file_and_parent_directory(self):
        payload = collector.project(self.fixture("status-authoritative.json"), "2026-09-10T15:00:00Z")
        with tempfile.TemporaryDirectory() as directory, mock.patch.object(collector.os, "fsync", wraps=os.fsync) as fsync:
            root = pathlib.Path(directory)
            path, _ = self.trusted_output(root)
            self.write_trusted(root, path, payload)
            self.assertEqual(fsync.call_count, 2)


if __name__ == "__main__":
    unittest.main()
