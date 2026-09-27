import json
import os
import pathlib
import subprocess
import tempfile
import textwrap
import time
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "deploy" / "verify-live-opencodego.sh"


class VerifyLiveOpencodegoTest(unittest.TestCase):
    def write_executable(self, path, body):
        path.write_text(textwrap.dedent(body).lstrip())
        path.chmod(0o700)

    def run_verify(
        self,
        service_log="service ok",
        projected_json=None,
        management_url=None,
        status_account_name="opencodego-1",
        status_json=None,
        journal_stalls=False,
        journal_timeout="1",
    ):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            bin_dir = root / "bin"
            bin_dir.mkdir()
            usage_dir = root / "usage"
            management_file = root / "management.key"
            dashboard_file = root / "dashboard.key"
            management_file.write_text("fixture-management-marker\n")
            dashboard_file.write_text("fixture-dashboard-marker\n")
            management_file.chmod(0o600)
            dashboard_file.chmod(0o600)
            curl_log = root / "curl-argv.log"
            self.write_executable(
                bin_dir / "curl",
                f"""
                #!/usr/bin/env python3
                import json, pathlib, sys
                with pathlib.Path({str(curl_log)!r}).open("a") as log:
                    log.write("\\n".join(sys.argv[1:]) + "\\n---\\n")
                args=sys.argv[1:]
                if "%{{http_code}}" in args:
                    print("401", end="")
                else:
                    status_json={status_json!r}
                    if status_json is not None:
                        print(status_json)
                        raise SystemExit
                    print(json.dumps({{
                        "plugin":"subscription-pool",
                        "status":"registered",
                        "version":"0.0.0-dev",
                        "generated_at":"2026-09-10T15:00:00Z",
                        "providers":{{
                            "opencode-go":{{
                                "provider":"opencode-go",
                                "status":"registered",
                                "credential_bound":True,
                                "observation_gaps":[],
                                "accounts":[{{
                                    "name":{status_account_name!r},
                                    "disabled":False,
                                    "windows":{{
                                        "five_hour":{{"known":True,"utilization":0.1,"exhausted":False,"resets_at":"2026-09-10T20:00:00Z","source":"poll","authoritative":True}},
                                        "weekly":{{"known":True,"utilization":0.2,"exhausted":False,"resets_at":"2026-09-16T00:00:00Z","source":"poll","authoritative":True}},
                                        "monthly":{{"known":True,"utilization":0.05,"exhausted":False,"resets_at":"2026-10-01T00:00:00Z","source":"poll","authoritative":True}}
                                    }}
                                }}]
                            }}
                        }}
                    }}))
                """,
            )
            journal_body = (
                """
                #!/usr/bin/env python3
                import time
                time.sleep(60)
                """
                if journal_stalls
                else f"""
                #!/usr/bin/env python3
                print({service_log!r})
                """
            )
            self.write_executable(bin_dir / "journalctl", journal_body)
            self.write_executable(
                bin_dir / "collector-opencodego",
                f"""
                #!/usr/bin/env python3
                import json, os, pathlib
                output=pathlib.Path({str(usage_dir / "opencode-go.json")!r})
                output.parent.mkdir(parents=True, exist_ok=True)
                os.chmod(output.parent, 0o700)
                projected_json={projected_json!r}
                if projected_json is not None:
                    output.write_text(projected_json)
                else:
                    output.write_text(json.dumps({{
                        "schemaVersion":1,"lane":"opencode-go","credential_bound":True,
                        "records":[{{"name":{status_account_name!r},"disabled":False,"windows":{{}}}}]
                    }}))
                os.chmod(output, 0o600)
                """,
            )
            env = os.environ.copy()
            env.update(
                {
                    "PATH": f"{bin_dir}:{env['PATH']}",
                    "CLIPROXY_MANAGEMENT_KEY_FILE": str(management_file),
                    "OPENCODE_GO_DASHBOARD_API_KEY_FILE": str(dashboard_file),
                    "CLIPROXY_USAGE_DIR": str(usage_dir),
                    "COLLECTOR_OPENCODEGO": str(bin_dir / "collector-opencodego"),
                    "CLIPROXY_JOURNAL_TIMEOUT": journal_timeout,
                    "HTTP_PROXY": "http://proxy.invalid:9876",
                    "HTTPS_PROXY": "http://proxy.invalid:9876",
                    "ALL_PROXY": "socks5://proxy.invalid:9876",
                    "NO_PROXY": "",
                }
            )
            if management_url is not None:
                env["CLIPROXY_MANAGEMENT_URL"] = management_url
            completed = subprocess.run([str(SCRIPT)], cwd=ROOT, env=env, text=True, capture_output=True)
            return completed, curl_log.read_text() if curl_log.exists() else ""

    def test_management_and_dashboard_keys_never_appear_in_curl_argv(self):
        completed, curl_argv = self.run_verify()
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assertNotIn("fixture-management-marker", curl_argv)
        self.assertNotIn("fixture-dashboard-marker", curl_argv)

    def test_rejects_unapproved_management_origin_before_curl(self):
        completed, curl_argv = self.run_verify(management_url="http://127.0.0.1:9999")
        self.assertNotEqual(completed.returncode, 0)
        self.assertEqual(curl_argv, "")
        self.assertIn("origin is not approved", completed.stderr)

    def test_authenticated_curl_is_time_size_and_proxy_bounded(self):
        completed, curl_argv = self.run_verify()
        self.assertEqual(completed.returncode, 0, completed.stderr)
        invocations = [part for part in curl_argv.split("---\n") if "management.curl" in part]
        self.assertEqual(len(invocations), 1)
        args = invocations[0].splitlines()
        self.assertEqual(args[0], "-q")
        self.assertIn("--max-time\n5", invocations[0])
        self.assertIn("--max-filesize\n1048576", invocations[0])
        self.assertIn("--max-redirs\n0", invocations[0])
        self.assertIn("--noproxy\n*", invocations[0])
        self.assertIn("--proxy\n", invocations[0])

    def test_rejects_unsafe_journal_timeout_before_curl(self):
        for value in ("0", "-1", "--help", "1 day", "nan", "inf"):
            with self.subTest(value=value):
                completed, curl_argv = self.run_verify(journal_timeout=value)
                self.assertNotEqual(completed.returncode, 0)
                self.assertEqual(curl_argv, "")
                self.assertIn("must be a positive duration", completed.stderr)

    def test_missing_provider_entry_fails(self):
        status = (
            '{"plugin":"subscription-pool","status":"registered","version":"0.0.0-dev",'
            '"generated_at":"2026-09-10T15:00:00Z","providers":{}}'
        )
        completed, _ = self.run_verify(status_json=status)
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("missing the opencode-go provider entry", completed.stderr)

    def test_stalling_journalctl_is_terminated_by_explicit_timeout(self):
        started = time.monotonic()
        completed, _ = self.run_verify(journal_stalls=True)
        elapsed = time.monotonic() - started
        self.assertNotEqual(completed.returncode, 0)
        self.assertLess(elapsed, 10)
        self.assertNotIn("fixture-management-marker", completed.stdout + completed.stderr)
        self.assertNotIn("fixture-dashboard-marker", completed.stdout + completed.stderr)

    def test_raw_authenticated_status_rejects_markers_before_projection(self):
        completed, _ = self.run_verify(status_account_name="fixture-dashboard-marker")
        self.assertNotEqual(completed.returncode, 0)
        self.assertNotIn("fixture-management-marker", completed.stdout + completed.stderr)
        self.assertNotIn("fixture-dashboard-marker", completed.stdout + completed.stderr)
        self.assertIn("authenticated status response failed confidential-value scan", completed.stderr)

    def test_bounded_scans_reject_markers_without_printing_them(self):
        for source, value in (
            ("service_log", "fixture-management-marker"),
            ("service_log", "fixture-dashboard-marker"),
        ):
            with self.subTest(source=source, value=value):
                completed, _ = self.run_verify(**{source: value})
                self.assertNotEqual(completed.returncode, 0)
                self.assertNotIn("fixture-management-marker", completed.stdout + completed.stderr)
                self.assertNotIn("fixture-dashboard-marker", completed.stdout + completed.stderr)

    def test_oversized_service_log_is_rejected_before_full_capture(self):
        completed, _ = self.run_verify(service_log="x" * 1_048_577)
        self.assertNotEqual(completed.returncode, 0)


if __name__ == "__main__":
    unittest.main()
