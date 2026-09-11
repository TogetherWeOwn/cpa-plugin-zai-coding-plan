import os
import pathlib
import subprocess
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]
RENDERER = ROOT / "deploy" / "render-config.go"


class RenderConfigTest(unittest.TestCase):
    def run_renderer(self, base, overlay, plan_key="fixture-plan-key", suffix="fixture-suffix"):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
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
                ["go", "run", str(RENDERER), str(base_path), str(overlay_path), str(candidate)],
                cwd=ROOT,
                env=env,
                text=True,
                capture_output=True,
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


if __name__ == "__main__":
    unittest.main()
