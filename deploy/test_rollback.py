import os
import pathlib
import subprocess
import tempfile
import textwrap
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "deploy" / "rollback.sh"


class RollbackSecurityTest(unittest.TestCase):
    def write_executable(self, path, body):
        path.write_text(textwrap.dedent(body).lstrip())
        path.chmod(0o700)

    def run_rollback(self, management_url):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            bin_dir = root / "bin"
            bin_dir.mkdir()
            config_dir = root / "config"
            config_dir.mkdir()
            config = config_dir / "config.yaml"
            backup = config_dir / "config.yaml.backup"
            key_file = root / "management.key"
            config.write_text("current\n")
            backup.write_text("backup\n")
            key_file.write_text("fixture-management-marker\n")
            config_dir.chmod(0o700)
            backup.chmod(0o600)
            key_file.chmod(0o600)
            curl_log = root / "curl.log"
            self.write_executable(
                bin_dir / "stat",
                """
                #!/usr/bin/env python3
                import pathlib, sys
                path=pathlib.Path(sys.argv[-1])
                format_value=sys.argv[sys.argv.index('-c') + 1]
                if format_value == '%u:%g:%a' and path.name == 'config':
                    print('0:0:700')
                elif format_value == '%u:%a' and path.name == 'config.yaml.backup':
                    print('0:600')
                else:
                    raise SystemExit(1)
                """,
            )
            self.write_executable(
                bin_dir / "curl",
                f"""
                #!/usr/bin/env python3
                import pathlib
                pathlib.Path({str(curl_log)!r}).write_text('called')
                """,
            )
            for name in ("systemctl",):
                self.write_executable(bin_dir / name, "#!/usr/bin/env bash\nexit 0\n")
            env = os.environ.copy()
            env.update(
                {
                    "PATH": f"{bin_dir}:{env['PATH']}",
                    "CLIPROXY_MANAGEMENT_URL": management_url,
                    "CLIPROXY_CONFIG": str(config),
                    "CLIPROXY_BACKUP": str(backup),
                    "CLIPROXY_MANAGEMENT_KEY_FILE": str(key_file),
                    "CLIPROXY_USAGE_DIR": str(root / "usage"),
                }
            )
            completed = subprocess.run([str(SCRIPT)], cwd=ROOT, env=env, text=True, capture_output=True)
            return completed, curl_log.exists()

    def test_rejects_unsafe_management_urls_before_credentialed_curl(self):
        cases = {
            "userinfo": "http://user@127.0.0.1:8317",
            "wrong origin": "http://127.0.0.1:9999",
            "non-loopback": "https://example.com:8317",
            "query": "http://127.0.0.1:8317?target=evil",
            "fragment": "http://127.0.0.1:8317#evil",
            "invalid port": "http://127.0.0.1:invalid",
        }
        for name, url in cases.items():
            with self.subTest(name=name):
                completed, curl_called = self.run_rollback(url)
                self.assertNotEqual(completed.returncode, 0)
                self.assertFalse(curl_called)
                self.assertRegex(completed.stderr, r"management URL|origin")


if __name__ == "__main__":
    unittest.main()
