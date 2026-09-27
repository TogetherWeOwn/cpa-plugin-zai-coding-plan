import importlib.util
import os
import pathlib
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "deploy" / "remove-usage-output.py"
SPEC = importlib.util.spec_from_file_location("remove_usage_output", SCRIPT)
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


class RemoveUsageOutputTest(unittest.TestCase):
    def remove(self, filesystem_root):
        MODULE.remove_usage_output(
            expected_uid=os.getuid(),
            filesystem_root=filesystem_root,
        )

    def make_usage(self, root):
        usage = root / "srv" / "cliproxy-usage"
        usage.mkdir(parents=True, mode=0o700)
        os.chmod(usage, 0o700)
        return usage

    def test_regular_output_is_removed_descriptor_relative(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            output = self.make_usage(root) / "zai.json"
            output.write_text("{}")
            self.remove(root)
            self.assertFalse(output.exists())

    def test_missing_output_is_a_noop(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            usage = self.make_usage(root)
            self.remove(root)
            self.assertTrue(usage.is_dir())

    def test_symlinked_ancestor_is_rejected_without_deleting_target(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            target = root / "target"
            target.mkdir(mode=0o700)
            output = target / "zai.json"
            output.write_text("keep")
            (root / "srv").symlink_to(target, target_is_directory=True)
            with self.assertRaisesRegex(RuntimeError, "could not be opened securely"):
                self.remove(root)
            self.assertEqual(output.read_text(), "keep")

    def test_symlink_output_is_rejected_without_deleting_target(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            usage = self.make_usage(root)
            target = root / "target.json"
            target.write_text("keep")
            (usage / "zai.json").symlink_to(target)
            with self.assertRaisesRegex(RuntimeError, "regular no-follow file"):
                self.remove(root)
            self.assertEqual(target.read_text(), "keep")
            self.assertTrue((usage / "zai.json").is_symlink())

    def test_arbitrary_output_path_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaisesRegex(ValueError, "must be exactly"):
                MODULE.remove_usage_output(
                    pathlib.Path("/etc/passwd"),
                    expected_uid=os.getuid(),
                    filesystem_root=pathlib.Path(directory),
                )


if __name__ == "__main__":
    unittest.main()
