#!/usr/bin/env python3
"""Create or secure the fixed collector output directory without following symlinks."""
from __future__ import annotations

import argparse
import os
import pathlib
import stat

TRUSTED_USAGE_DIR = pathlib.Path("/srv/cliproxy-usage")


def _directory_flags() -> int:
    return os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0)


def _open_directory_path(path: pathlib.Path, filesystem_root: pathlib.Path) -> int:
    flags = _directory_flags()
    try:
        directory_fd = os.open(filesystem_root, flags)
    except OSError as exc:
        raise RuntimeError("filesystem root could not be opened securely") from exc
    try:
        for component in path.parts[1:]:
            try:
                next_fd = os.open(component, flags, dir_fd=directory_fd)
            except OSError as exc:
                raise RuntimeError("usage directory path could not be opened securely") from exc
            os.close(directory_fd)
            directory_fd = next_fd
        return directory_fd
    except Exception:
        os.close(directory_fd)
        raise


def prepare_usage_dir(
    path: pathlib.Path,
    *,
    expected_uid: int = 0,
    expected_gid: int = 0,
    trusted_path: pathlib.Path = TRUSTED_USAGE_DIR,
    filesystem_root: pathlib.Path = pathlib.Path("/"),
) -> None:
    if not path.is_absolute() or path != trusted_path or trusted_path != TRUSTED_USAGE_DIR:
        raise ValueError(f"usage directory must be exactly {TRUSTED_USAGE_DIR}")
    if not filesystem_root.is_absolute():
        raise ValueError("filesystem root must be absolute")

    parent_path = pathlib.Path(*path.parts[:-1])
    parent_fd = _open_directory_path(parent_path, filesystem_root)
    try:
        try:
            metadata = os.stat(path.name, dir_fd=parent_fd, follow_symlinks=False)
        except FileNotFoundError:
            try:
                os.mkdir(path.name, 0o700, dir_fd=parent_fd)
            except FileExistsError:
                pass
        except OSError as exc:
            raise RuntimeError("usage directory could not be inspected securely") from exc
        else:
            if stat.S_ISLNK(metadata.st_mode):
                raise RuntimeError("usage directory must not be a symlink")
            if not stat.S_ISDIR(metadata.st_mode):
                raise RuntimeError("usage directory path exists but is not a directory")

        try:
            directory_fd = os.open(path.name, _directory_flags(), dir_fd=parent_fd)
        except OSError as exc:
            raise RuntimeError("usage directory could not be opened without following symlinks") from exc
    finally:
        os.close(parent_fd)

    try:
        metadata = os.fstat(directory_fd)
        if not stat.S_ISDIR(metadata.st_mode):
            raise RuntimeError("usage directory is not a directory")
        os.fchown(directory_fd, expected_uid, expected_gid)
        os.fchmod(directory_fd, 0o700)
        metadata = os.fstat(directory_fd)
        if (metadata.st_uid, metadata.st_gid, stat.S_IMODE(metadata.st_mode)) != (
            expected_uid,
            expected_gid,
            0o700,
        ):
            raise RuntimeError("usage directory ownership or mode validation failed")

        verification_fd = _open_directory_path(path, filesystem_root)
        try:
            verification = os.fstat(verification_fd)
        finally:
            os.close(verification_fd)
        if (verification.st_dev, verification.st_ino) != (metadata.st_dev, metadata.st_ino):
            raise RuntimeError("usage directory pathname changed during preparation")
    finally:
        os.close(directory_fd)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("path", type=pathlib.Path)
    args = parser.parse_args()
    prepare_usage_dir(args.path)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
