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

    def test_registry_source_is_immutable_digest_verified_and_bounded(self):
        config = CONFIG.read_text()
        readme = README.read_text()
        registry = readme.split("Verify the exact registry bytes", 1)[1].split("## Exact host install", 1)[0]
        self.assertNotIn("/main/registry.json", config)
        self.assertIn(f"/{PINNED_COMMIT}/registry.json", config)
        self.assertIn(PINNED_DIGEST, readme)
        self.assertIn("sha256sum --check", registry)
        for option in ("-q", "--connect-timeout 2", "--max-time 10", "--max-filesize 1048576"):
            self.assertIn(option, registry)
        self.assertIn('test -s "$registry"', registry)
        self.assertEqual(hashlib.sha256(REGISTRY.read_bytes()).hexdigest(), PINNED_DIGEST)

    def test_config_replacement_is_rendered_validated_same_directory_fsynced_and_atomic(self):
        install = README.read_text().split("## Exact host install and validation", 1)[1].split("## Rollback", 1)[0]
        renderer = (ROOT / "deploy" / "render-config.go").read_text()
        self.assertIn('mktemp --tmpdir="$config_dir"', install)
        self.assertIn('go run "$repo/deploy/render-config.go" "$config" "$repo/deploy/config.yaml.tmpl" "$candidate"', install)
        self.assertIn('test -s "$candidate"', install)
        self.assertIn("yaml.NewDecoder", renderer)
        self.assertIn("must contain exactly one YAML document", renderer)
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
            "-q",
            "--fail",
            "--fail-early",
            "--max-redirs 0",
            "--connect-timeout 2",
            "--max-time 10",
            "--max-filesize 1048576",
            "--noproxy '*'",
            "--proxy ''",
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
        delete = rollback.split("config_dir=", 1)[0]
        for option in (
            "-q",
            "--fail-early",
            "--max-redirs 0",
            "--connect-timeout 2",
            "--max-time 5",
            "--max-filesize 1048576",
            "--noproxy '*'",
            "--proxy ''",
            '--output "$delete_response"',
            '--stderr "$delete_error"',
            "--write-out '%{http_code}'",
        ):
            self.assertIn(option, delete)
        self.assertNotIn("--fail-with-body", delete)
        self.assertIn("continuing rollback", delete)
        self.assertIn('python3 "$repo/deploy/rollback-delete-policy.py"', delete)
        policy = (ROOT / "deploy" / "rollback-delete-policy.py").read_text()
        self.assertIn('http_status == 404', policy)
        self.assertIn('response.get("error") == "plugin_not_found"', policy)
        self.assertLess(delete.index("continuing rollback"), rollback.index("config_dir="))
        self.assertIn("umask 077", rollback)
        self.assertRegex(delete, r"grep -Eq '[^']+' \"\$delete_error\"")

    def test_post_install_readiness_is_bounded_and_version_pinned(self):
        install = README.read_text().split("usage_dir=/srv/cliproxy-usage", 1)[1].split("## Rollback", 1)[0]
        self.assertIn("for attempt in $(seq 1 20)", install)
        self.assertIn("CLIPROXY_EXPECTED_PLUGIN_VERSION=0.1.0", install)
        self.assertIn("sleep 1", install)
        self.assertIn('test "$ready" = 1', install)

    def test_runbook_passes_both_secret_marker_files_to_live_verification(self):
        text = README.read_text()
        self.assertIn("CLIPROXY_MANAGEMENT_KEY_FILE=", text)
        self.assertIn("ZAI_CODING_PLAN_KEY_FILE=", text)
        self.assertRegex(text, re.escape("dashboard") + ".*" + re.escape("service-log"))


if __name__ == "__main__":
    unittest.main()
