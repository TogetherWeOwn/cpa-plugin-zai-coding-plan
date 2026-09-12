import os
import pathlib
import subprocess
import tempfile
import textwrap
import time
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "deploy" / "verify-live.sh"


class VerifyLiveTest(unittest.TestCase):
    def write_executable(self, path, body):
        path.write_text(textwrap.dedent(body).lstrip())
        path.chmod(0o700)

    def run_verify(
        self,
        dashboard="dashboard ok",
        service_log="service ok",
        projected="projected ok",
        projected_json=None,
        management_url=None,
        status_plan="pro",
        status_plan_json=None,
        dashboard_json=None,
        journal_stalls=False,
        journal_timeout="1",
        router_dry_run=None,
        canary_command=None,
        python_optimize=False,
    ):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            bin_dir = root / "bin"
            bin_dir.mkdir()
            usage_dir = root / "usage"
            management_file = root / "management.key"
            plan_file = root / "plan.key"
            management_file.write_text("fixture-management-marker\n")
            plan_file.write_text("fixture-plan-marker\n")
            management_file.chmod(0o600)
            plan_file.chmod(0o600)
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
                elif {"dashboard"!r} in " ".join(args):
                    dashboard_json={dashboard_json!r}
                    if dashboard_json is not None:
                        print(dashboard_json)
                    else:
                        print(json.dumps({{"status": {dashboard!r}}}))
                else:
                    status_json={status_plan_json!r}
                    if status_json is not None:
                        print(status_json)
                        raise SystemExit
                    print(json.dumps({{
                        "plugin":"zai-coding-plan",
                        "status":"registered",
                        "version":"0.0.0-dev",
                        "generated_at":"2026-09-10T15:00:00Z",
                        "accounts":[{{
                            "name":"zai-pro-1","key_suffix":"redacted","plan":{status_plan!r},
                            "five_hour_utilization":0.1,"weekly_utilization":0.2,
                            "five_hour_resets_at":None,"weekly_resets_at":None,
                            "quota_source":"estimate","quota_observed_at":"2026-09-10T15:00:00Z",
                            "quota_age_seconds":1,"quota_stale":False,"offpeak":False,"health":"healthy",
                            "estimator_complete_since":"2026-09-10T15:00:00Z","delivery_warning":False,
                            "persistence_warning":False,"unknown_model_warning":False,
                            "heuristic_dedup_warning":False,"dedup_mode":"bounded_hash_heuristic"
                        }}]
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
                bin_dir / "collector-zai",
                f"""
                #!/usr/bin/env python3
                import json, os, pathlib
                output=pathlib.Path({str(usage_dir / "zai.json")!r})
                output.parent.mkdir(parents=True, exist_ok=True)
                os.chmod(output.parent, 0o700)
                projected_json={projected_json!r}
                if projected_json is not None:
                    output.write_text(projected_json)
                else:
                    output.write_text(json.dumps({{
                        "schemaVersion":1,"lane":"zai","records":[{{"key_suffix":"redacted","note":{projected!r}}}]
                    }}))
                os.chmod(output, 0o600)
                """,
            )
            env = os.environ.copy()
            env.update(
                {
                    "PATH": f"{bin_dir}:{env['PATH']}",
                    "CLIPROXY_MANAGEMENT_KEY_FILE": str(management_file),
                    "ZAI_CODING_PLAN_KEY_FILE": str(plan_file),
                    "CLIPROXY_USAGE_DIR": str(usage_dir),
                    "CLIPROXY_DASHBOARD_URL": "http://127.0.0.1/dashboard",
                    "COLLECTOR_ZAI": str(bin_dir / "collector-zai"),
                    "CLIPROXY_JOURNAL_TIMEOUT": journal_timeout,
                    "HTTP_PROXY": "http://proxy.invalid:9876",
                    "HTTPS_PROXY": "http://proxy.invalid:9876",
                    "ALL_PROXY": "socks5://proxy.invalid:9876",
                    "NO_PROXY": "",
                }
            )
            if python_optimize:
                env["PYTHONOPTIMIZE"] = "1"
            if management_url is not None:
                env["CLIPROXY_MANAGEMENT_URL"] = management_url
            if router_dry_run is not None:
                env["CLIPROXY_ROUTER_DRY_RUN"] = router_dry_run
            if canary_command is not None:
                env["CLIPROXY_CANARY_COMMAND"] = canary_command
            completed = subprocess.run([str(SCRIPT)], cwd=ROOT, env=env, text=True, capture_output=True)
            return completed, curl_log.read_text() if curl_log.exists() else ""

    def test_management_key_never_appears_in_curl_argv(self):
        completed, curl_argv = self.run_verify()
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assertNotIn("fixture-management-marker", curl_argv)

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

    def test_unauthenticated_loopback_curl_disables_ambient_config_and_proxy(self):
        completed, curl_argv = self.run_verify()
        self.assertEqual(completed.returncode, 0, completed.stderr)
        invocations = [part for part in curl_argv.split("---\n") if "%{http_code}" in part]
        self.assertEqual(len(invocations), 1)
        args = invocations[0].splitlines()
        self.assertEqual(args[0], "-q")
        self.assertIn("--noproxy\n*", invocations[0])
        self.assertIn("--proxy\n", invocations[0])

    def test_rejects_unsafe_journal_timeout_before_curl(self):
        for value in ("0", "-1", "--help", "1 day", "nan", "inf"):
            with self.subTest(value=value):
                completed, curl_argv = self.run_verify(journal_timeout=value)
                self.assertNotEqual(completed.returncode, 0)
                self.assertEqual(curl_argv, "")
                self.assertIn("must be a positive duration", completed.stderr)

    def test_schema_checks_still_fail_under_python_optimize(self):
        completed, _ = self.run_verify(status_plan="fixture-plan-marker", python_optimize=True)
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("confidential-value scan", completed.stderr)

    def test_malformed_status_schema_fails_under_python_optimize(self):
        completed, _ = self.run_verify(status_plan_json='{"plugin":"zai-coding-plan"}', python_optimize=True)
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("unexpected top-level fields", completed.stderr)

    def test_timeout_uses_option_terminator(self):
        text = SCRIPT.read_text()
        self.assertIn('-- "$CLIPROXY_JOURNAL_TIMEOUT" \\\n  journalctl', text)

    @staticmethod
    def unicode_escape(value):
        return "".join(f"\\u{ord(character):04x}" for character in value)

    def test_unicode_escaped_status_markers_are_rejected_after_json_decode(self):
        escaped = self.unicode_escape("fixture-plan-marker")
        status = (
            '{"plugin":"zai-coding-plan","status":"registered","version":"0.0.0-dev",'
            '"generated_at":"2026-09-10T15:00:00Z","accounts":[{'
            '"name":"zai-pro-1","key_suffix":"redacted","plan":"' + escaped + '",'
            '"five_hour_utilization":0.1,"weekly_utilization":0.2,'
            '"five_hour_resets_at":null,"weekly_resets_at":null,"quota_source":"estimate",'
            '"quota_observed_at":"2026-09-10T15:00:00Z","quota_age_seconds":1,'
            '"quota_stale":false,"offpeak":false,"health":"healthy",'
            '"estimator_complete_since":"2026-09-10T15:00:00Z","delivery_warning":false,'
            '"persistence_warning":false,"unknown_model_warning":false,'
            '"heuristic_dedup_warning":false,"dedup_mode":"bounded_hash_heuristic"}]}'
        )
        completed, _ = self.run_verify(status_plan_json=status)
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("decoded confidential-value scan", completed.stderr)

    def test_unicode_escaped_dashboard_suffix_is_rejected_after_json_decode(self):
        escaped = self.unicode_escape("marker")
        completed, _ = self.run_verify(dashboard_json='{"note":"' + escaped + '"}')
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("decoded confidential-value scan", completed.stderr)

    def test_unicode_escaped_projected_full_marker_is_rejected_after_json_decode(self):
        escaped = self.unicode_escape("fixture-management-marker")
        projected = '{"schemaVersion":1,"lane":"zai","records":[{"key_suffix":"redacted","note":"' + escaped + '"}]}'
        completed, _ = self.run_verify(projected_json=projected)
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("decoded confidential-value scan", completed.stderr)

    def test_unicode_escaped_projected_plan_suffix_is_rejected_after_json_decode(self):
        escaped = self.unicode_escape("marker")
        projected = '{"schemaVersion":1,"lane":"zai","records":[{"key_suffix":"redacted","note":"' + escaped + '"}]}'
        completed, _ = self.run_verify(projected_json=projected)
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("decoded confidential-value scan", completed.stderr)

    def test_oversized_service_log_is_rejected_before_full_capture(self):
        completed, _ = self.run_verify(service_log="x" * 1_048_577)
        self.assertNotEqual(completed.returncode, 0)

    def test_stalling_journalctl_is_terminated_by_explicit_timeout(self):
        started = time.monotonic()
        completed, _ = self.run_verify(journal_stalls=True)
        elapsed = time.monotonic() - started
        self.assertNotEqual(completed.returncode, 0)
        self.assertLess(elapsed, 10)
        self.assertNotIn("fixture-management-marker", completed.stdout + completed.stderr)
        self.assertNotIn("fixture-plan-marker", completed.stdout + completed.stderr)

    def test_router_dry_run_stdout_is_bounded_during_execution(self):
        command = "python3 -c 'import os; os.write(1, b\"x\" * 1048577)'"
        completed, curl_argv = self.run_verify(router_dry_run=command)
        self.assertNotEqual(completed.returncode, 0)
        self.assertEqual(curl_argv, "")
        self.assertIn("output exceeded 1 MiB per stream", completed.stderr)

    def test_router_dry_run_stderr_is_bounded_during_execution(self):
        command = "python3 -c 'import os; os.write(2, b\"x\" * 1048577)'"
        completed, curl_argv = self.run_verify(router_dry_run=command)
        self.assertNotEqual(completed.returncode, 0)
        self.assertEqual(curl_argv, "")
        self.assertIn("output exceeded 1 MiB per stream", completed.stderr)

    def test_canary_stdout_and_stderr_accept_bounded_output(self):
        command = "python3 -c 'import os; os.write(1, b\"ok\"); os.write(2, b\"warning\")'"
        completed, _ = self.run_verify(canary_command=command)
        self.assertEqual(completed.returncode, 0, completed.stderr)

    def test_raw_authenticated_status_rejects_markers_before_projection(self):
        for marker in ("fixture-management-marker", "fixture-plan-marker", "marker"):
            with self.subTest(marker=marker):
                completed, _ = self.run_verify(status_plan=marker)
                self.assertNotEqual(completed.returncode, 0)
                self.assertNotIn("fixture-management-marker", completed.stdout + completed.stderr)
                self.assertNotIn("fixture-plan-marker", completed.stdout + completed.stderr)
                self.assertIn("authenticated status response failed confidential-value scan", completed.stderr)

    def test_bounded_scans_reject_full_or_suffix_markers_without_printing_them(self):
        for source, value in (
            ("projected", "fixture-plan-marker"),
            ("dashboard", "fixture-plan-marker"),
            ("service_log", "fixture-management-marker"),
            ("projected", "marker"),
            ("dashboard", "marker"),
            ("service_log", "marker"),
        ):
            with self.subTest(source=source, value=value):
                completed, _ = self.run_verify(**{source: value})
                self.assertNotEqual(completed.returncode, 0)
                self.assertNotIn("fixture-plan-marker", completed.stdout + completed.stderr)
                self.assertNotIn("fixture-management-marker", completed.stdout + completed.stderr)


if __name__ == "__main__":
    unittest.main()
