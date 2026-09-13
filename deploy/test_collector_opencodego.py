import http.server
import importlib.util
import json
import math
import os
import pathlib
import stat
import tempfile
import threading
import unittest
from unittest import mock

MODULE_PATH = pathlib.Path(__file__).with_name("collector-opencodego.py")
SPEC = importlib.util.spec_from_file_location("collector_opencodego", MODULE_PATH)
collector = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(collector)
FIXTURES = pathlib.Path(__file__).with_name("testdata")


class CollectorOpencodegoTest(unittest.TestCase):
    def fixture(self, name):
        return json.loads((FIXTURES / name).read_text())

    def test_projects_required_field_names(self):
        out = collector.project(self.fixture("opencodego-status-authoritative.json"), "2026-09-10T15:00:00Z")
        self.assertEqual(out["lane"], "opencode-go")
        self.assertEqual(set(out["records"][0]), {"name", "disabled", "windows"})
        self.assertEqual(set(out["records"][0]["windows"]), collector.WINDOW_KINDS)
        self.assertTrue(out["records"][0]["windows"]["five_hour"]["known"])
        self.assertEqual(out["credential_bound"], True)

    def test_projects_unbound_windows_as_unknown(self):
        out = collector.project(self.fixture("opencodego-status-unbound.json"), "2026-09-10T15:00:00Z")
        self.assertEqual(out["credential_bound"], False)
        for kind in collector.WINDOW_KINDS:
            window = out["records"][0]["windows"][kind]
            self.assertFalse(window["known"])
            self.assertNotIn("utilization", window)
            self.assertNotIn("resets_at", window)

    def test_reconfigure_rejected_marks_all_windows_unknown(self):
        value = self.fixture("opencodego-status-authoritative.json")
        value["providers"]["opencode-go"]["status"] = "reconfigure_rejected"
        value["providers"]["opencode-go"]["validation_error"] = "invalid replacement configuration"
        out = collector.project(value, "2026-09-10T15:00:00Z")
        for kind in collector.WINDOW_KINDS:
            self.assertFalse(out["records"][0]["windows"][kind]["known"])
            self.assertFalse(out["records"][0]["windows"][kind]["exhausted"])

    def test_missing_provider_entry_fails_closed(self):
        value = self.fixture("opencodego-status-authoritative.json")
        del value["providers"]["opencode-go"]
        with self.assertRaisesRegex(ValueError, "missing the opencode-go entry"):
            collector.project(value, "2026-09-10T15:00:00Z")

    def test_wrong_provider_identity_rejected(self):
        value = self.fixture("opencodego-status-authoritative.json")
        value["providers"]["opencode-go"]["provider"] = "zai"
        with self.assertRaises(ValueError):
            collector.project(value, "2026-09-10T15:00:00Z")

    def test_bounds_and_redacts_every_persisted_string(self):
        value = self.fixture("opencodego-status-unbound.json")
        value["providers"]["opencode-go"]["observation_gaps"][0] = "management-key=fixture-management-marker"
        out = collector.project(value, "2026-09-10T15:00:00Z")
        self.assertEqual(out["observation_gaps"][0], "redacted")
        value = self.fixture("opencodego-status-unbound.json")
        value["providers"]["opencode-go"]["observation_gaps"][0] = "bare-fixture-plan-marker"
        out = collector.project(value, "2026-09-10T15:00:00Z", ("bare-fixture-plan-marker",))
        self.assertEqual(out["observation_gaps"][0], "redacted")
        for field in ("name",):
            with self.subTest(field=field):
                value = self.fixture("opencodego-status-authoritative.json")
                value["providers"]["opencode-go"]["accounts"][0][field] = "x" * (collector.MAX_STRING_BYTES + 1)
                with self.assertRaises(ValueError):
                    collector.project(value, "2026-09-10T15:00:00Z")

    def test_rejects_null_names_and_nonfinite_numbers(self):
        cases = (
            ("name", None),
        )
        for field, value in cases:
            status = self.fixture("opencodego-status-authoritative.json")
            status["providers"]["opencode-go"]["accounts"][0][field] = value
            with self.subTest(field=field, value=value), self.assertRaises(ValueError):
                collector.project(status, "2026-09-10T15:00:00Z")
        for bad in (math.nan, math.inf, -math.inf):
            status = self.fixture("opencodego-status-authoritative.json")
            status["providers"]["opencode-go"]["accounts"][0]["windows"]["five_hour"]["utilization"] = bad
            with self.subTest(value=bad), self.assertRaises(ValueError):
                collector.project(status, "2026-09-10T15:00:00Z")

    def test_rejects_utilization_or_resets_at_while_known_is_false(self):
        status = self.fixture("opencodego-status-unbound.json")
        status["providers"]["opencode-go"]["accounts"][0]["windows"]["five_hour"]["utilization"] = 0.5
        with self.assertRaises(ValueError):
            collector.project(status, "2026-09-10T15:00:00Z")
        status = self.fixture("opencodego-status-unbound.json")
        status["providers"]["opencode-go"]["accounts"][0]["windows"]["five_hour"]["resets_at"] = "2026-09-08T13:00:00Z"
        with self.assertRaises(ValueError):
            collector.project(status, "2026-09-10T15:00:00Z")

    def test_rejects_invalid_timestamps(self):
        for field in ("generated_at", "observedAt", "resets_at"):
            with self.subTest(field=field):
                status = self.fixture("opencodego-status-authoritative.json")
                observed_at = "2026-09-10T15:00:00Z"
                if field == "generated_at":
                    status[field] = "not-a-timestamp"
                elif field == "observedAt":
                    observed_at = "not-a-timestamp"
                else:
                    status["providers"]["opencode-go"]["accounts"][0]["windows"]["five_hour"]["resets_at"] = "not-a-timestamp"
                with self.assertRaises(ValueError):
                    collector.project(status, observed_at)

    def test_rejects_unknown_top_level_and_window_fields_and_non_rfc_json(self):
        status = self.fixture("opencodego-status-authoritative.json")
        status["extra"] = "value"
        with self.assertRaises(ValueError):
            collector.project(status, "2026-09-10T15:00:00Z")
        status = self.fixture("opencodego-status-authoritative.json")
        status["providers"]["opencode-go"]["accounts"][0]["windows"]["five_hour"]["extra"] = "value"
        with self.assertRaises(ValueError):
            collector.project(status, "2026-09-10T15:00:00Z")
        for constant in (b"NaN", b"Infinity", b"-Infinity"):
            raw = b'{"value":' + constant + b"}"
            with self.subTest(constant=constant), self.assertRaisesRegex(ValueError, "non-RFC JSON"):
                collector.load_json_strict(raw)

    def test_rejects_wrong_window_key_set(self):
        status = self.fixture("opencodego-status-authoritative.json")
        del status["providers"]["opencode-go"]["accounts"][0]["windows"]["monthly"]
        with self.assertRaises(ValueError):
            collector.project(status, "2026-09-10T15:00:00Z")

    def test_rejects_non_string_and_nested_values_in_allowed_fields(self):
        cases = (
            ("name", 123),
        )
        for field, value in cases:
            status = self.fixture("opencodego-status-authoritative.json")
            status["providers"]["opencode-go"]["accounts"][0][field] = value
            with self.subTest(field=field), self.assertRaises(ValueError):
                collector.project(status, "2026-09-10T15:00:00Z")
        status = self.fixture("opencodego-status-authoritative.json")
        status["providers"]["opencode-go"]["credential_bound"] = "true"
        with self.assertRaises(ValueError):
            collector.project(status, "2026-09-10T15:00:00Z")

    def test_rejects_secret_like_fields_but_allows_credential_bound(self):
        for field in ("api_key", "authorization", "credential", "key_hash", "identity", "dashboard_api_key"):
            value = self.fixture("opencodego-status-authoritative.json")
            value["providers"]["opencode-go"]["accounts"][0][field] = "fixture-secret"
            with self.subTest(field=field), self.assertRaises(ValueError):
                collector.project(value, "2026-09-10T15:00:00Z")
        value = self.fixture("opencodego-status-authoritative.json")
        out = collector.project(value, "2026-09-10T15:00:00Z")
        self.assertIn("credential_bound", out)

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
            "https://management.internal/v0/management/plugins/subscription-pool/status",
            ("https://management.internal",),
        )
        for suffix in ("?next=1", "#fragment"):
            with self.subTest(suffix=suffix), self.assertRaises(ValueError):
                collector.validate_management_url(
                    "http://127.0.0.1:8317/v0/management/plugins/subscription-pool/status" + suffix,
                    collector.DEFAULT_ALLOWED_ORIGINS,
                )

    def test_rejects_redirect_without_forwarding_authorization(self):
        seen = []

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                seen.append((self.path, self.headers.get("Authorization")))
                if self.path == "/v0/management/plugins/subscription-pool/status":
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
                    f"{origin}/v0/management/plugins/subscription-pool/status",
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
            [("/v0/management/plugins/subscription-pool/status", "Bearer fixture-management-marker")],
        )

    def trusted_output(self, root):
        parent = root / "srv" / "cliproxy-usage"
        parent.mkdir(parents=True)
        parent.chmod(0o700)
        return pathlib.Path("/srv/cliproxy-usage/opencode-go.json"), parent

    def write_trusted(self, root, path, payload, expected_owner_uid=None):
        collector.write_atomic(
            path,
            payload,
            expected_owner_uid=os.getuid() if expected_owner_uid is None else expected_owner_uid,
            trusted_output=pathlib.Path("/srv/cliproxy-usage/opencode-go.json"),
            filesystem_root=root,
        )

    def test_atomic_write_enforces_initial_and_replacement_file_mode(self):
        payload = collector.project(self.fixture("opencodego-status-authoritative.json"), "2026-09-10T15:00:00Z")
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            path, parent = self.trusted_output(root)
            self.write_trusted(root, path, payload)
            output = parent / "opencode-go.json"
            self.assertEqual(stat.S_IMODE(parent.stat().st_mode), 0o700)
            self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o600)
            os.chmod(output, 0o644)
            self.write_trusted(root, path, payload)
            self.assertEqual(stat.S_IMODE(parent.stat().st_mode), 0o700)
            self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o600)

    def test_atomic_write_rejects_wrong_root_path_without_chmod(self):
        payload = collector.project(self.fixture("opencodego-status-authoritative.json"), "2026-09-10T15:00:00Z")
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            path, parent = self.trusted_output(root)
            parent.chmod(0o755)
            wrong = pathlib.Path("/srv/caller-selected/opencode-go.json")
            with self.assertRaisesRegex(ValueError, "exactly"):
                self.write_trusted(root, wrong, payload)
            self.assertEqual(stat.S_IMODE(parent.stat().st_mode), 0o755)
            self.assertFalse((root / wrong.relative_to("/")).exists())

    def test_atomic_write_rejects_symlinked_ancestor_component(self):
        payload = collector.project(self.fixture("opencodego-status-authoritative.json"), "2026-09-10T15:00:00Z")
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            victim = root / "victim"
            victim.mkdir()
            victim.chmod(0o700)
            (root / "srv").symlink_to(victim, target_is_directory=True)
            path = pathlib.Path("/srv/cliproxy-usage/opencode-go.json")
            with self.assertRaisesRegex(ValueError, "could not be opened securely"):
                self.write_trusted(root, path, payload)
            self.assertFalse((victim / "cliproxy-usage" / "opencode-go.json").exists())

    def test_atomic_write_rejects_wrong_owner_or_mode_without_chmod(self):
        payload = collector.project(self.fixture("opencodego-status-authoritative.json"), "2026-09-10T15:00:00Z")
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
        payload = collector.project(self.fixture("opencodego-status-authoritative.json"), "2026-09-10T15:00:00Z")
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            path, parent = self.trusted_output(root)
            victim = root / "victim.json"
            victim.write_text("unchanged")
            (parent / "opencode-go.json").symlink_to(victim)
            with self.assertRaisesRegex(ValueError, "must not be a symlink"):
                self.write_trusted(root, path, payload)
            self.assertEqual(victim.read_text(), "unchanged")

    def test_atomic_write_fsyncs_file_and_parent_directory(self):
        payload = collector.project(self.fixture("opencodego-status-authoritative.json"), "2026-09-10T15:00:00Z")
        with tempfile.TemporaryDirectory() as directory, mock.patch.object(collector.os, "fsync", wraps=os.fsync) as fsync:
            root = pathlib.Path(directory)
            path, _ = self.trusted_output(root)
            self.write_trusted(root, path, payload)
            self.assertEqual(fsync.call_count, 2)


if __name__ == "__main__":
    unittest.main()
