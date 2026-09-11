import os
import pathlib
import subprocess
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]


class RenderConfigTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.binary_directory = tempfile.TemporaryDirectory()
        cls.renderer = pathlib.Path(cls.binary_directory.name) / "render-config"
        subprocess.run(
            ["go", "build", "-buildvcs=false", "-trimpath", "-o", str(cls.renderer), "./deploy/render-config.go"],
            cwd=ROOT,
            check=True,
            timeout=30,
        )

    @classmethod
    def tearDownClass(cls):
        cls.binary_directory.cleanup()

    def run_renderer(self, base, overlay, plan_key="fixture-plan-key", suffix="fixture-suffix"):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            root.chmod(0o700)
            base_path = root / "base.yaml"
            overlay_path = root / "overlay.yaml"
            candidate = root / "candidate.yaml"
            key_file = root / "plan.key"
            suffix_file = root / "suffix.key"
            base_path.write_text(base)
            overlay_path.write_text(overlay)
            candidate.touch(mode=0o600)
            key_file.write_text(plan_key + "\n")
            suffix_file.write_text(suffix + "\n")
            base_path.chmod(0o600)
            overlay_path.chmod(0o600)
            candidate.chmod(0o600)
            key_file.chmod(0o600)
            suffix_file.chmod(0o600)
            env = os.environ.copy()
            env.update(
                {
                    "ZAI_CODING_PLAN_KEY_FILE": str(key_file),
                    "ZAI_CODING_PLAN_KEY_SUFFIX_FILE": str(suffix_file),
                }
            )
            completed = subprocess.run(
                [str(self.renderer), str(base_path), str(overlay_path), str(candidate)],
                cwd=ROOT,
                env=env,
                text=True,
                capture_output=True,
                timeout=5,
            )
            rendered = candidate.read_text()
            return completed, rendered

    def test_deterministically_merges_overlay_and_renders_complete_secret_scalars(self):
        base = "port: 8317\nplugins:\n  enabled: false\n  dir: /old\n"
        overlay = (
            "claude-api-key:\n"
            "  - api-key: ${ZAI_CODING_PLAN_KEY}\n"
            "plugins:\n"
            "  enabled: true\n"
            "  configs:\n"
            "    zai-coding-plan:\n"
            "      accounts:\n"
            "        - key-suffix: ${ZAI_CODING_PLAN_KEY_SUFFIX}\n"
        )
        first, first_rendered = self.run_renderer(base, overlay)
        second, second_rendered = self.run_renderer(base, overlay)
        self.assertEqual(first.returncode, 0, first.stderr)
        self.assertEqual(second.returncode, 0, second.stderr)
        self.assertEqual(first_rendered, second_rendered)
        self.assertIn("port: 8317", first_rendered)
        self.assertIn("enabled: true", first_rendered)
        self.assertIn("dir: /old", first_rendered)
        self.assertIn("fixture-plan-key", first_rendered)
        self.assertIn("fixture-suffix", first_rendered)
        self.assertNotIn("${ZAI_", first_rendered)

    def test_preserves_unrelated_provider_and_source_sequences(self):
        base = (
            "claude-api-key:\n"
            "  - api-key: existing-claude-key\n"
            "    base-url: https://existing.invalid/anthropic\n"
            "    prefix: existing\n"
            "openai-compatibility:\n"
            "  - name: existing-openai\n"
            "    base-url: https://existing.invalid/openai\n"
            "    api-key-entries:\n"
            "      - api-key: existing-openai-key\n"
            "plugins:\n"
            "  store-sources:\n"
            "    - https://existing.invalid/registry.json\n"
        )
        overlay = (ROOT / "deploy" / "config.yaml.tmpl").read_text()
        completed, rendered = self.run_renderer(base, overlay)
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assertLess(rendered.index("prefix: existing"), rendered.index("prefix: zai"))
        self.assertLess(rendered.index("name: existing-openai"), rendered.index("name: zai-coding-plan"))
        self.assertLess(
            rendered.index("https://existing.invalid/registry.json"),
            rendered.index("https://raw.githubusercontent.com/TogetherWeOwn/cpa-plugin-zai-coding-plan/"),
        )
        self.assertEqual(rendered.count("prefix: existing"), 1)
        self.assertEqual(rendered.count("prefix: zai"), 1)
        self.assertEqual(rendered.count("name: existing-openai"), 1)
        self.assertEqual(rendered.count("name: zai-coding-plan"), 1)

    def test_rejects_empty_or_invalid_candidate_input_without_rendering_secrets(self):
        for overlay in ("", "plugins: ["):
            with self.subTest(overlay=overlay):
                completed, rendered = self.run_renderer("port: 8317\n", overlay)
                self.assertNotEqual(completed.returncode, 0)
                self.assertEqual(rendered, "")
                self.assertNotIn("fixture-plan-key", completed.stdout + completed.stderr)

    def test_rejects_placeholder_embedded_inside_another_scalar(self):
        completed, rendered = self.run_renderer(
            "port: 8317\n",
            "value: prefix-${ZAI_CODING_PLAN_KEY}\n",
        )
        self.assertNotEqual(completed.returncode, 0)
        self.assertEqual(rendered, "")
        self.assertIn("complete YAML scalar", completed.stderr)
        self.assertNotIn("fixture-plan-key", completed.stdout + completed.stderr)

    def test_rejects_oversized_secret_file(self):
        completed, rendered = self.run_renderer("port: 8317\n", "value: ${ZAI_CODING_PLAN_KEY}\n", "x" * (64 * 1024 + 1))
        self.assertNotEqual(completed.returncode, 0)
        self.assertEqual(rendered, "")
        self.assertIn("could not be read", completed.stderr)

    def test_replaces_same_identity_entries_without_duplicates(self):
        pinned_source = "https://raw.githubusercontent.com/TogetherWeOwn/cpa-plugin-zai-coding-plan/5c758c04acbd9367d1c8fff1342bf651847dcac2/registry.json"
        base = (
            "claude-api-key:\n"
            "  - api-key: stale-claude-key\n"
            "    base-url: https://stale.invalid/anthropic\n"
            "    prefix: zai\n"
            "openai-compatibility:\n"
            "  - name: zai-coding-plan\n"
            "    base-url: https://stale.invalid/openai\n"
            "    api-key-entries:\n"
            "      - api-key: stale-openai-key\n"
            "plugins:\n"
            "  store-sources:\n"
            f"    - {pinned_source}\n"
        )
        overlay = (ROOT / "deploy" / "config.yaml.tmpl").read_text()
        completed, rendered = self.run_renderer(base, overlay)
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assertNotIn("stale-claude-key", rendered)
        self.assertNotIn("stale-openai-key", rendered)
        self.assertEqual(rendered.count("prefix: zai"), 1)
        self.assertEqual(rendered.count("name: zai-coding-plan"), 1)
        self.assertEqual(rendered.count(pinned_source), 1)

    def test_rejects_candidate_in_different_directory(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            root.chmod(0o700)
            other = root / "other"
            other.mkdir(mode=0o700)
            base = root / "base.yaml"
            overlay = root / "overlay.yaml"
            candidate = other / "candidate.yaml"
            key = root / "key"
            suffix = root / "suffix"
            base.write_text("port: 8317\n")
            overlay.write_text("value: ${ZAI_CODING_PLAN_KEY}\n")
            candidate.touch(mode=0o600)
            key.write_text("fixture-key\n")
            suffix.write_text("fixture-suffix\n")
            for path in (base, overlay, candidate, key, suffix):
                path.chmod(0o600)
            env = os.environ.copy()
            env.update({"ZAI_CODING_PLAN_KEY_FILE": str(key), "ZAI_CODING_PLAN_KEY_SUFFIX_FILE": str(suffix)})
            completed = subprocess.run(
                [str(self.renderer), str(base), str(overlay), str(candidate)],
                cwd=ROOT,
                env=env,
                text=True,
                capture_output=True,
                timeout=5,
            )
            self.assertNotEqual(completed.returncode, 0)
            self.assertIn("must share one directory", completed.stderr)

    def test_rejects_candidate_symlink_without_truncating_target(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            root.chmod(0o700)
            base = root / "base.yaml"
            overlay = root / "overlay.yaml"
            candidate = root / "candidate.yaml"
            victim = root / "victim.yaml"
            key = root / "key"
            suffix = root / "suffix"
            base.write_text("port: 8317\n")
            overlay.write_text("value: ${ZAI_CODING_PLAN_KEY}\n")
            victim.write_text("unchanged\n")
            candidate.symlink_to(victim)
            key.write_text("fixture-key\n")
            suffix.write_text("fixture-suffix\n")
            for path in (base, overlay, victim, key, suffix):
                path.chmod(0o600)
            env = os.environ.copy()
            env.update({"ZAI_CODING_PLAN_KEY_FILE": str(key), "ZAI_CODING_PLAN_KEY_SUFFIX_FILE": str(suffix)})
            completed = subprocess.run(
                [str(self.renderer), str(base), str(overlay), str(candidate)],
                cwd=ROOT,
                env=env,
                text=True,
                capture_output=True,
                timeout=5,
            )
            self.assertNotEqual(completed.returncode, 0)
            self.assertEqual(victim.read_text(), "unchanged\n")

    def test_rejects_insecure_candidate_without_truncating_it(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            root.chmod(0o700)
            base = root / "base.yaml"
            overlay = root / "overlay.yaml"
            candidate = root / "candidate.yaml"
            key = root / "key"
            suffix = root / "suffix"
            base.write_text("port: 8317\n")
            overlay.write_text("value: ${ZAI_CODING_PLAN_KEY}\n")
            candidate.write_text("unchanged\n")
            key.write_text("fixture-key\n")
            suffix.write_text("fixture-suffix\n")
            for path in (base, overlay, key, suffix):
                path.chmod(0o600)
            candidate.chmod(0o644)
            env = os.environ.copy()
            env.update({"ZAI_CODING_PLAN_KEY_FILE": str(key), "ZAI_CODING_PLAN_KEY_SUFFIX_FILE": str(suffix)})
            completed = subprocess.run(
                [str(self.renderer), str(base), str(overlay), str(candidate)],
                cwd=ROOT,
                env=env,
                text=True,
                capture_output=True,
                timeout=5,
            )
            self.assertNotEqual(completed.returncode, 0)
            self.assertEqual(candidate.read_text(), "unchanged\n")


if __name__ == "__main__":
    unittest.main()
