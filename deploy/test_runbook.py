import hashlib
import pathlib
import re
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]
README = ROOT / "deploy" / "README.md"
CONFIG = ROOT / "deploy" / "config.yaml.tmpl"
REGISTRY = ROOT / "registry.json"
PINNED_COMMIT = "RELEASE_SHA"
PINNED_DIGEST = "REGISTRY_SHA256"


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
        self.assertIn("v0.2.0/registry.json", config)
        self.assertIn('release_dir=/path/to/downloaded-v0.2.0-release-assets', readme)
        self.assertIn('"$release_dir/registry.json"', readme)
        self.assertIn('grep -F "  registry.json" "$release_dir/checksums.txt"', readme)
        self.assertIn("sha256sum --check --status", readme)
        self.assertIn('release_sha=$(<"$release_dir/release-sha.txt")', readme)
        self.assertEqual(len(hashlib.sha256(REGISTRY.read_bytes()).hexdigest()), 64)

    def test_config_replacement_is_same_directory_fsynced_and_atomic(self):
        install = README.read_text().split("## Exact host install and validation", 1)[1].split("## Rollback", 1)[0]
        self.assertIn('mktemp --tmpdir="$config_dir"', install)
        self.assertIn("os.fsync(handle.fileno())", install)
        self.assertIn('mv -T "$candidate" "$config"', install)
        self.assertIn("os.fsync(directory_fd)", install)
        self.assertIn("systemctl reload cliproxy.service || systemctl restart cliproxy.service", install)

    def test_rollback_restores_atomically_and_reloads_running_service(self):
        rollback = README.read_text().split("## Rollback", 1)[1]
        self.assertIn("repo=/home/ubuntu/cpa-plugin-zai-coding-plan", rollback)
        self.assertIn("config=/home/ubuntu/cliproxy/config.yaml", rollback)
        self.assertIn("backup=/home/ubuntu/cliproxy/config.yaml.pre-zai-", rollback)
        self.assertLess(rollback.index("backup="), rollback.index('install -m 0600 "$backup" "$restored"'))
        self.assertIn('mktemp --tmpdir="$config_dir"', rollback)
        self.assertIn('mv -T "$restored" "$config"', rollback)
        self.assertIn("os.fsync(directory_fd)", rollback)
        self.assertIn("systemctl reload cliproxy.service || systemctl restart cliproxy.service", rollback)

    def test_rollback_removes_usage_output_with_no_follow_helper(self):
        rollback = README.read_text().split("## Rollback", 1)[1]
        helper = (ROOT / "deploy" / "remove-usage-output.py").read_text()
        self.assertIn('python3 "$repo/deploy/remove-usage-output.py"', rollback)
        self.assertNotIn("rm -f /srv/cliproxy-usage/zai.json", rollback)
        self.assertIn("O_NOFOLLOW", helper)
        self.assertIn("dir_fd=directory_fd", helper)
        self.assertIn("follow_symlinks=False", helper)
        self.assertIn("os.unlink(path.name, dir_fd=directory_fd)", helper)

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

    def test_usage_directory_is_validated_before_any_mutating_action(self):
        install = README.read_text().split("usage_dir=/srv/cliproxy-usage", 1)[1].split(
            "CLIPROXY_MANAGEMENT_KEY_FILE=", 1
        )[0]
        self.assertIn('python3 "$repo/deploy/prepare-usage-dir.py" "$usage_dir"', install)
        self.assertNotIn("install -d", install)
        self.assertNotIn("chmod", install)
        self.assertNotIn("chown", install)

    def test_rollback_delete_is_bounded_and_suppresses_response_body(self):
        rollback = README.read_text().split("## Rollback", 1)[1]
        delete = rollback
        self.assertLess(delete.index("install -m 0600"), delete.index("-X DELETE"))
        for option in (
            "--fail",
            "--fail-early",
            "--max-redirs 0",
            "--connect-timeout 2",
            "--max-time 5",
            "--max-filesize 1048576",
            '--output "$delete_response"',
            '--stderr "$delete_error"',
        ):
            self.assertIn(option, delete)
        self.assertNotIn("--fail-with-body", delete)
        self.assertIn("response body suppressed", delete)
        self.assertIn("umask 077", rollback)
        self.assertRegex(delete, r"grep -Eq '[^']+' \"\$delete_error\"")

    def test_runbook_passes_both_secret_marker_files_to_live_verification(self):
        text = README.read_text()
        self.assertIn("CLIPROXY_MANAGEMENT_KEY_FILE=", text)
        self.assertIn("ZAI_CODING_PLAN_KEY_FILE=", text)
        self.assertRegex(text, re.escape("dashboard") + ".*" + re.escape("service-log"))


if __name__ == "__main__":
    unittest.main()
