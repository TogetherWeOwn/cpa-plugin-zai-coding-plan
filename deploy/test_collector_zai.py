import importlib.util
import json
import pathlib
import unittest

MODULE_PATH = pathlib.Path(__file__).with_name("collector-zai.py")
SPEC = importlib.util.spec_from_file_location("collector_zai", MODULE_PATH)
collector = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(collector)
FIXTURES = pathlib.Path(__file__).with_name("testdata")


class CollectorZaiTest(unittest.TestCase):
    def fixture(self, name):
        return json.loads((FIXTURES / name).read_text())

    def test_projects_required_field_names(self):
        out = collector.project(self.fixture("status-authoritative.json"), "2026-09-10T15:00:00Z")
        self.assertEqual(out["lane"], "zai")
        self.assertEqual(out["records"][0]["quota_source"], "quota_api")
        self.assertEqual(set(out["records"][0]), collector.EXPECTED_ACCOUNT_FIELDS - {"quota_error"})

    def test_accepts_bounded_redacted_fallback_error(self):
        out = collector.project(self.fixture("status-fallback.json"), "2026-09-10T15:00:00Z")
        self.assertEqual(out["records"][0]["quota_error"], "quota request failed")

    def test_rejects_secret_like_fields_and_nonredacted_suffix(self):
        for field in ("api_key", "authorization", "credential", "key_hash", "identity"):
            value = self.fixture("status-authoritative.json")
            value["accounts"][0][field] = "fixture-secret"
            with self.subTest(field=field), self.assertRaises(ValueError):
                collector.project(value, "2026-09-10T15:00:00Z")
        value = self.fixture("status-authoritative.json")
        value["accounts"][0]["key_suffix"] = "last-four"
        with self.assertRaises(ValueError):
            collector.project(value, "2026-09-10T15:00:00Z")


if __name__ == "__main__":
    unittest.main()
