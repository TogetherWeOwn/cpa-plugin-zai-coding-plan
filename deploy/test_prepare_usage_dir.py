import importlib.util
import os
import pathlib
import tempfile
import unittest
from unittest import mock

ROOT = pathlib.Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "deploy" / "prepare-usage-dir.py"
SPEC = importlib.util.spec_from_file_location("prepare_usage_dir", SCRIPT)
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


class PrepareUsageDirTest(unittest.TestCase):
    def prepare(self, filesystem_root, **kwargs):
        usage = MODULE.TRUSTED_USAGE_DIR
        MODULE.prepare_usage_dir(
            usage,
            expected_uid=os.getuid(),
            expected_gid=os.getgid(),
            trusted_path=usage,
            filesystem_root=filesystem_root,
            **kwargs,
        )
        return filesystem_root / "srv" / "cliproxy-usage"

    def test_existing_symlink_is_rejected_without_mutating_target(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            (root / "srv").mkdir()
            target = root / "target"
            target.mkdir(mode=0o755)
            marker = target / "marker"
            marker.write_text("unchanged")
            usage = root / "srv" / "cliproxy-usage"
            usage.symlink_to(target, target_is_directory=True)

            before_mode = target.stat().st_mode & 0o777
            with self.assertRaisesRegex(RuntimeError, "must not be a symlink"):
                self.prepare(root)

            self.assertTrue(usage.is_symlink())
            self.assertEqual(target.stat().st_mode & 0o777, before_mode)
            self.assertEqual(marker.read_text(), "unchanged")

    def test_real_directory_is_created_and_secured(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            (root / "srv").mkdir()
            usage = self.prepare(root)
            metadata = usage.stat()
            self.assertTrue(usage.is_dir())
            self.assertFalse(usage.is_symlink())
            self.assertEqual(metadata.st_mode & 0o777, 0o700)
            self.assertEqual((metadata.st_uid, metadata.st_gid), (os.getuid(), os.getgid()))

    def test_arbitrary_target_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            arbitrary = pathlib.Path("/etc")
            with self.assertRaisesRegex(ValueError, "must be exactly"):
                MODULE.prepare_usage_dir(
                    arbitrary,
                    expected_uid=os.getuid(),
                    expected_gid=os.getgid(),
                    trusted_path=MODULE.TRUSTED_USAGE_DIR,
                    filesystem_root=root,
                )

    def test_symlinked_ancestor_is_rejected_without_creating_target(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            target = root / "target"
            target.mkdir()
            (root / "srv").symlink_to(target, target_is_directory=True)

            with self.assertRaisesRegex(RuntimeError, "could not be opened securely"):
                self.prepare(root)

            self.assertFalse((target / "cliproxy-usage").exists())

    def test_pathname_replacement_is_rejected_before_success(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            srv = root / "srv"
            srv.mkdir()
            original = srv / "cliproxy-usage"
            original.mkdir()
            replacement = srv / "replacement"
            replacement.mkdir()
            real_open = MODULE._open_directory_path
            calls = 0

            def replace_before_verification(path, filesystem_root):
                nonlocal calls
                calls += 1
                if calls == 2:
                    original.rename(srv / "held-original")
                    replacement.rename(original)
                return real_open(path, filesystem_root)

            with mock.patch.object(MODULE, "_open_directory_path", side_effect=replace_before_verification):
                with self.assertRaisesRegex(RuntimeError, "pathname changed"):
                    self.prepare(root)


if __name__ == "__main__":
    unittest.main()
