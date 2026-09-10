import hashlib
import pathlib
import re
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]
README = ROOT / "deploy" / "README.md"
CONFIG = ROOT / "deploy" / "config.yaml.tmpl"
REGISTRY = ROOT / "registry.json"
PINNED_COMMIT = "5c758c04acbd9367d1c8fff1342bf651847dcac2"
PINNED_DIGEST = "86800494fa9606971c22ee5f08872dbc3c02280ad65fe9a88fdeaf9063c5db0d"


class RunbookSecurityTest(unittest.TestCase):
    def test_management_key_is_not_interpolated_into_curl_arguments(self):
        text = README.read_text() + (ROOT / "deploy" / "verify-live.sh").read_text()
        self.assertNotRegex(text, r'-H\s+["\']Authorization: Bearer \$\{?management_key')
        self.assertNotIn("management_key=$(<", text)
        self.assertIn('--config "$curl_config"', text)

    def test_registry_source_is_immutable_and_digest_verified(self):
        config = CONFIG.read_text()
        readme = README.read_text()
        self.assertNotIn("/main/registry.json", config)
        self.assertIn(f"/{PINNED_COMMIT}/registry.json", config)
        self.assertIn(PINNED_DIGEST, readme)
        self.assertIn("sha256sum --check", readme)
        self.assertEqual(hashlib.sha256(REGISTRY.read_bytes()).hexdigest(), PINNED_DIGEST)

    def test_config_replacement_is_same_directory_fsynced_and_atomic(self):
        install = README.read_text().split("## Exact host install and validation", 1)[1].split("## Rollback", 1)[0]
        self.assertIn('mktemp --tmpdir="$config_dir"', install)
        self.assertIn("os.fsync(handle.fileno())", install)
        self.assertIn('mv -T "$candidate" "$config"', install)
        self.assertIn("os.fsync(directory_fd)", install)
        self.assertIn("systemctl reload cliproxy.service || systemctl restart cliproxy.service", install)

    def test_rollback_restores_atomically_and_reloads_running_service(self):
        rollback = README.read_text().split("## Rollback", 1)[1]
        self.assertIn("config=/home/ubuntu/cliproxy/config.yaml", rollback)
        self.assertIn("backup=/home/ubuntu/cliproxy/config.yaml.pre-zai-", rollback)
        self.assertLess(rollback.index("backup="), rollback.index('install -m 0600 "$backup" "$restored"'))
        self.assertIn('mktemp --tmpdir="$config_dir"', rollback)
        self.assertIn('mv -T "$restored" "$config"', rollback)
        self.assertIn("os.fsync(directory_fd)", rollback)
        self.assertIn("systemctl reload cliproxy.service || systemctl restart cliproxy.service", rollback)

    def test_plugin_install_curl_is_bounded_and_suppresses_error_bodies(self):
        install = README.read_text().split("# After config reload exposes the custom source", 1)[1].split("usage_dir=", 1)[0]
        for option in (
            "--fail",
            "--fail-early",
            "--max-redirs 0",
            "--connect-timeout 2",
            "--max-time 10",
            "--max-filesize 1048576",
            '--output "$install_response"',
            '--stderr "$install_error"',
        ):
            self.assertIn(option, install)
        self.assertNotIn("--fail-with-body", install)
        self.assertIn("response body suppressed", install)
        self.assertIn("plugin install response did not confirm the expected release", install)

    def test_runbook_passes_both_secret_marker_files_to_live_verification(self):
        text = README.read_text()
        self.assertIn("CLIPROXY_MANAGEMENT_KEY_FILE=", text)
        self.assertIn("ZAI_CODING_PLAN_KEY_FILE=", text)
        self.assertRegex(text, re.escape("dashboard") + ".*" + re.escape("service-log"))


if __name__ == "__main__":
    unittest.main()
