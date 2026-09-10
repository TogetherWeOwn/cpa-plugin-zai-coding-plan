import os
import pathlib
import subprocess
import tempfile
import textwrap
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "deploy" / "verify-live.sh"


class VerifyLiveTest(unittest.TestCase):
    def write_executable(self, path, body):
        path.write_text(textwrap.dedent(body).lstrip())
        path.chmod(0o700)

    def run_verify(self, dashboard="dashboard ok", service_log="service ok", projected="projected ok"):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            bin_dir = root / "bin"
            bin_dir.mkdir()
            usage_dir = root / "usage"
            management_file = root / "management.key"
            plan_file = root / "plan.key"
            management_file.write_text("fixture-management-marker\n")
            plan_file.write_text("fixture-plan-marker\n")
            curl_log = root / "curl-argv.log"
            self.write_executable(
                bin_dir / "curl",
                f"""
                #!/usr/bin/env python3
                import json, os, pathlib, sys
                with pathlib.Path({str(curl_log)!r}).open("a") as log:
                    log.write("\\n".join(sys.argv[1:]) + "\\n---\\n")
                args=sys.argv[1:]
                if "%{{http_code}}" in args:
                    print("401", end="")
                elif {"dashboard"!r} in " ".join(args):
                    print({dashboard!r})
                else:
                    print(json.dumps({{
                        "plugin":"zai-coding-plan",
                        "status":"registered",
                        "accounts":[{{
                            "name":"zai-pro-1","key_suffix":"redacted","plan":"pro",
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
            self.write_executable(
                bin_dir / "journalctl",
                f"""
                #!/usr/bin/env python3
                print({service_log!r})
                """,
            )
            self.write_executable(
                bin_dir / "collector-zai",
                f"""
                #!/usr/bin/env python3
                import json, os, pathlib, sys
                output=pathlib.Path(sys.argv[sys.argv.index("--output") + 1])
                output.parent.mkdir(parents=True, exist_ok=True)
                os.chmod(output.parent, 0o700)
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
                }
            )
            completed = subprocess.run([str(SCRIPT)], cwd=ROOT, env=env, text=True, capture_output=True)
            return completed, curl_log.read_text()

    def test_management_key_never_appears_in_curl_argv(self):
        completed, curl_argv = self.run_verify()
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assertNotIn("fixture-management-marker", curl_argv)

    def test_oversized_service_log_is_rejected_before_full_capture(self):
        completed, _ = self.run_verify(service_log="x" * 1_048_577)
        self.assertNotEqual(completed.returncode, 0)

    def test_bounded_scans_reject_management_or_plan_markers_without_printing_them(self):
        for source, value in (
            ("projected", "fixture-plan-marker"),
            ("dashboard", "fixture-plan-marker"),
            ("service_log", "fixture-management-marker"),
        ):
            with self.subTest(source=source):
                completed, _ = self.run_verify(**{source: value})
                self.assertNotEqual(completed.returncode, 0)
                self.assertNotIn(value, completed.stdout + completed.stderr)


if __name__ == "__main__":
    unittest.main()
