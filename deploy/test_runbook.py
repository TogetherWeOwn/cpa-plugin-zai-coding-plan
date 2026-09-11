import hashlib
import os
import pathlib
import re
import shlex
import subprocess
import tempfile
import textwrap
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]
README = ROOT / "deploy" / "README.md"
CONFIG = ROOT / "deploy" / "config.yaml.tmpl"
REGISTRY = ROOT / "registry.json"
PINNED_COMMIT = "5c758c04acbd9367d1c8fff1342bf651847dcac2"
PINNED_DIGEST = "86800494fa9606971c22ee5f08872dbc3c02280ad65fe9a88fdeaf9063c5db0d"


def staging_recipe():
    text = README.read_text()
    section = text.split("Run the credential-free checks", 1)[1].split("Verify the exact registry bytes", 1)[0]
    return section.split("```bash", 1)[1].split("```", 1)[0].strip()


class RunbookSecurityTest(unittest.TestCase):
    def test_management_key_is_not_interpolated_into_curl_arguments(self):
        text = README.read_text() + (ROOT / "deploy" / "verify-live.sh").read_text()
        self.assertNotRegex(text, r'-H\s+["\']Authorization: Bearer \$\{?management_key')
        self.assertNotIn("management_key=$(<", text)
        self.assertIn('--config "$curl_config"', text)

    def test_staging_recipe_stops_before_publication_on_command_failure(self):
        commands = ("git", "stat", "find", "timeout", "go", "install")
        failure_cases = (
            ("sha", "git", "rev-parse HEAD"),
            ("clean-tree", "git", "status --porcelain --untracked-files=all"),
            ("ownership", "stat", "-c %U:%G"),
            ("blob-hash", "git", "hash-object"),
            ("blob-reference", "git", "rev-parse REPLACE_WITH_REVIEWED_PR_HEAD:deploy/"),
            ("build", "timeout", "--signal=TERM"),
            ("install", "install", "-o root -g root -m 0755"),
            ("enumeration", "find", "-xdev -type f -print"),
        )
        script = staging_recipe().replace("./deploy/acceptance_local.py", ":")

        for name, fail_command, fail_match in failure_cases:
            with self.subTest(name=name), tempfile.TemporaryDirectory() as directory:
                root = pathlib.Path(directory)
                bin_dir = root / "bin"
                bin_dir.mkdir()
                staging_parent = root / "libexec"
                staging_parent.mkdir()
                published = root / "published"
                trace = root / "trace"
                for command in commands:
                    (bin_dir / command).symlink_to("dispatcher")
                dispatcher = bin_dir / "dispatcher"
                dispatcher.write_text(
                    textwrap.dedent(
                        """\
                        #!/bin/sh
                        set -eu
                        command_name=$(basename "$0")
                        printf '%s %s\\n' "$command_name" "$*" >>"$TRACE"
                        if test "$command_name" = "$FAIL_COMMAND" && case "$*" in *"$FAIL_MATCH"*) true;; *) false;; esac; then
                          exit 41
                        fi
                        case "$command_name:$*" in
                          "git:rev-parse HEAD") printf '%s\\n' REPLACE_WITH_REVIEWED_PR_HEAD ;;
                          "git:status --porcelain --untracked-files=all") : ;;
                          "git:show "*) printf 'artifact\\n' ;;
                          "git:hash-object "*) printf 'blob\\n' ;;
                          "git:rev-parse REPLACE_WITH_REVIEWED_PR_HEAD:deploy/"*) printf 'blob\\n' ;;
                          "stat:-c %U:%G "*) printf 'root:root\\n' ;;
                          "stat:-c %a "*) printf '755\\n' ;;
                          "find:"*) command /usr/bin/find "$@" ;;
                          "timeout:"*)
                            output=
                            previous=
                            for argument in "$@"; do
                              if test "$previous" = "-o"; then output=$argument; fi
                              previous=$argument
                            done
                            test -n "$output"
                            : >"$output"
                            ;;
                          "go:"*) : ;;
                          "install:"*)
                            destination=
                            for argument in "$@"; do destination=$argument; done
                            : >"$destination"
                            ;;
                        esac
                        """
                    )
                )
                dispatcher.chmod(0o755)
                wrapper = root / "run.sh"
                wrapper.write_text(
                    script.replace("staging=/usr/local/libexec/cliproxy/zai-dogfood", f"staging={shlex.quote(str(published))}")
                )
                wrapper.chmod(0o755)
                env = os.environ.copy()
                env.update(
                    {
                        "PATH": f"{bin_dir}:/usr/bin:/bin",
                        "TRACE": str(trace),
                        "FAIL_COMMAND": fail_command,
                        "FAIL_MATCH": fail_match,
                    }
                )
                completed = subprocess.run(["bash", str(wrapper)], cwd=ROOT, env=env, text=True, capture_output=True)
                command_trace = trace.read_text() if trace.exists() else "no trace"
                self.assertEqual(completed.returncode, 41, completed.stdout + completed.stderr + command_trace)
                self.assertIn(fail_command, command_trace)
                self.assertFalse(published.exists(), command_trace)

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

    def test_config_replacement_uses_built_bounded_renderer_and_atomic_replace(self):
        install = README.read_text().split("## Exact host install and validation", 1)[1].split("## Rollback", 1)[0]
        preflight = README.read_text().split("Run the credential-free checks", 1)[1].split("Verify the exact registry bytes", 1)[0]
        renderer = (ROOT / "deploy" / "render-config.go").read_text()
        self.assertRegex(preflight, r"```bash\s+set -euo pipefail\s+\./deploy/acceptance_local\.py")
        self.assertIn("renderer_build=$(mktemp -d)", preflight)
        self.assertIn("expected_deploy_commit=REPLACE_WITH_REVIEWED_PR_HEAD", preflight)
        self.assertIn('actual_deploy_commit=$(git rev-parse HEAD)', preflight)
        self.assertIn('test "$actual_deploy_commit" = "$expected_deploy_commit"', preflight)
        self.assertIn('working_tree_status=$(git status --porcelain --untracked-files=all)', preflight)
        self.assertIn('test -z "$working_tree_status"', preflight)
        self.assertIn('git show "$expected_deploy_commit:deploy/$artifact"', preflight)
        self.assertIn('git hash-object "$staging_tmp/$artifact"', preflight)
        self.assertIn('git rev-parse "$expected_deploy_commit:deploy/$artifact"', preflight)
        self.assertIn('staging_parent=$(dirname "$staging")', preflight)
        self.assertIn('staging_parent_owner=$(stat -c %U:%G "$staging_parent")', preflight)
        self.assertIn('test "$staging_parent_owner" = root:root', preflight)
        self.assertIn('mktemp -d --tmpdir="$staging_parent"', preflight)
        self.assertIn('mv -T "$staging_tmp" "$staging"', preflight)
        self.assertIn("timeout --signal=TERM --kill-after=2s 60s", preflight)
        for setting in ("GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0", "-mod=readonly"):
            self.assertIn(setting, preflight)
        self.assertIn('go build -mod=readonly -buildvcs=false -trimpath', preflight)
        self.assertIn('"$renderer_build/render-config" "$staging_tmp/render-config"', preflight)
        self.assertIn('staging_tmp_owner=$(stat -c %U:%G "$staging_tmp")', preflight)
        self.assertIn('test "$staging_tmp_owner" = root:root', preflight)
        self.assertIn('staging_tmp_mode=$(stat -c %a "$staging_tmp")', preflight)
        self.assertIn('test "$((8#$staging_tmp_mode & 8#022))" = 0', preflight)
        self.assertNotIn("go run", install)
        self.assertNotIn("/home/ubuntu/cpa-plugin-zai-coding-plan", install)
        self.assertIn('staging=/usr/local/libexec/cliproxy/zai-dogfood', install)
        self.assertIn('"$staging/config.yaml.tmpl"', install)
        self.assertIn('"$staging/prepare-usage-dir.py"', install)
        self.assertIn('"$staging/verify-live.sh"', install)
        self.assertIn('test ! -L "$renderer"', install)
        self.assertIn('test -f "$renderer"', install)
        self.assertIn('test -x "$renderer"', install)
        self.assertIn('test "$(stat -c %U:%G:%a "$renderer")" = root:root:755', install)
        self.assertIn('timeout --signal=TERM --kill-after=2s 10s "$renderer"', install)
        self.assertIn('mktemp --tmpdir="$config_dir"', install)
        self.assertIn('test -s "$candidate"', install)
        self.assertIn("unix.Openat", renderer)
        self.assertIn("unix.O_NOFOLLOW", renderer)
        self.assertIn("must share one directory", renderer)
        self.assertIn("os.fsync(handle.fileno())", install)
        self.assertIn('mv -T "$candidate" "$config"', install)
        self.assertIn("os.fsync(directory_fd)", install)
        self.assertIn("systemctl reload cliproxy.service || systemctl restart cliproxy.service", install)

    def test_rollback_restores_atomically_and_reloads_running_service(self):
        rollback = README.read_text().split("## Rollback", 1)[1]
        self.assertIn("staging=/usr/local/libexec/cliproxy/zai-dogfood", rollback)
        self.assertNotIn("/home/ubuntu/cpa-plugin-zai-coding-plan", rollback)
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
        self.assertIn('"$staging/remove-usage-output.py"', rollback)
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
        self.assertIn('"$staging/prepare-usage-dir.py" "$usage_dir"', install)
        self.assertNotIn("install -d", install)
        self.assertNotIn("chmod", install)
        self.assertNotIn("chown", install)

    def test_rollback_delete_restarts_retries_and_verifies_removal(self):
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
        self.assertIn('"$staging/rollback-delete-policy.py"', delete)
        policy = (ROOT / "deploy" / "rollback-delete-policy.py").read_text()
        self.assertIn('response.get("error") == "plugin_not_found"', policy)
        self.assertIn('response.get("error") == "plugin_delete_requires_restart"', policy)
        self.assertIn("return 10", policy)
        self.assertIn('systemctl restart cliproxy.service\n  delete_plugin', rollback)
        self.assertGreaterEqual(rollback.count("delete_plugin"), 3)
        self.assertIn("/v0/management/plugins/zai-coding-plan/config", rollback)
        self.assertIn('test "$verify_status" = 404', rollback)
        self.assertIn('rollback-delete-policy.py" 0 "$verify_status"', rollback)
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
