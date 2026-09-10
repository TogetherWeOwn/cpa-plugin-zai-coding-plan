import http.server
import importlib.util
import json
import os
import pathlib
import stat
import tempfile
import threading
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

    def test_atomic_write_enforces_modes_on_initial_and_replacement(self):
        payload = collector.project(self.fixture("status-authoritative.json"), "2026-09-10T15:00:00Z")
        with tempfile.TemporaryDirectory() as directory:
            parent = pathlib.Path(directory) / "collector"
            path = parent / "zai.json"
            collector.write_atomic(path, payload)
            self.assertEqual(stat.S_IMODE(parent.stat().st_mode), 0o700)
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)
            os.chmod(parent, 0o755)
            os.chmod(path, 0o644)
            collector.write_atomic(path, payload)
            self.assertEqual(stat.S_IMODE(parent.stat().st_mode), 0o700)
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)

    def test_atomic_write_fsyncs_file_and_parent_directory(self):
        payload = collector.project(self.fixture("status-authoritative.json"), "2026-09-10T15:00:00Z")
        with tempfile.TemporaryDirectory() as directory, mock.patch.object(collector.os, "fsync", wraps=os.fsync) as fsync:
            collector.write_atomic(pathlib.Path(directory) / "collector" / "zai.json", payload)
            self.assertEqual(fsync.call_count, 2)


if __name__ == "__main__":
    unittest.main()
