import pathlib
import subprocess
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "deploy" / "rollback-delete-policy.py"


class RollbackDeletePolicyTest(unittest.TestCase):
    def run_policy(self, curl_exit, http_status, body):
        with tempfile.TemporaryDirectory() as directory:
            response = pathlib.Path(directory) / "response.json"
            response.write_text(body)
            return subprocess.run(
                ["python3", str(SCRIPT), str(curl_exit), str(http_status), str(response)],
                cwd=ROOT,
                text=True,
                capture_output=True,
            )

    def test_accepts_success_and_idempotent_plugin_not_found(self):
        for curl_exit, status, body in (
            (0, 204, ""),
            (0, 404, '{"error":"plugin_not_found"}'),
            (7, 0, ""),
            (0, 503, "upstream unavailable"),
        ):
            with self.subTest(curl_exit=curl_exit, status=status):
                completed = self.run_policy(curl_exit, status, body)
                self.assertEqual(completed.returncode, 0, completed.stderr)
                if body:
                    self.assertNotIn(body, completed.stdout + completed.stderr)

    def test_rejects_unexpected_404_without_printing_body(self):
        body = '{"error":"permission_denied","secret":"fixture-management-marker"}'
        completed = self.run_policy(0, 404, body)
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("unexpected 404", completed.stderr)
        self.assertNotIn("fixture-management-marker", completed.stdout + completed.stderr)


if __name__ == "__main__":
    unittest.main()
