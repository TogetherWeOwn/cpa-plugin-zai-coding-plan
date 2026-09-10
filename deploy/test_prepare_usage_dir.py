import importlib.util
import os
import pathlib
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "deploy" / "prepare-usage-dir.py"
SPEC = importlib.util.spec_from_file_location("prepare_usage_dir", SCRIPT)
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


class PrepareUsageDirTest(unittest.TestCase):
    def test_existing_symlink_is_rejected_without_mutating_target(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            target = root / "target"
            target.mkdir(mode=0o755)
            marker = target / "marker"
            marker.write_text("unchanged")
            usage = root / "cliproxy-usage"
            usage.symlink_to(target, target_is_directory=True)

            before_mode = target.stat().st_mode & 0o777
            with self.assertRaisesRegex(RuntimeError, "must not be a symlink"):
                MODULE.prepare_usage_dir(
                    usage,
                    expected_uid=os.getuid(),
                    expected_gid=os.getgid(),
                )

            self.assertTrue(usage.is_symlink())
            self.assertEqual(target.stat().st_mode & 0o777, before_mode)
            self.assertEqual(marker.read_text(), "unchanged")

    def test_real_directory_is_created_and_secured(self):
        with tempfile.TemporaryDirectory() as directory:
            usage = pathlib.Path(directory) / "cliproxy-usage"
            MODULE.prepare_usage_dir(
                usage,
                expected_uid=os.getuid(),
                expected_gid=os.getgid(),
            )
            metadata = usage.stat()
            self.assertTrue(usage.is_dir())
            self.assertFalse(usage.is_symlink())
            self.assertEqual(metadata.st_mode & 0o777, 0o700)
            self.assertEqual((metadata.st_uid, metadata.st_gid), (os.getuid(), os.getgid()))


if __name__ == "__main__":
    unittest.main()
