#!/usr/bin/env python3
"""Create or secure the collector output directory without following symlinks."""
from __future__ import annotations

import argparse
import os
import pathlib
import stat


def prepare_usage_dir(path: pathlib.Path, *, expected_uid: int = 0, expected_gid: int = 0) -> None:
    if not path.is_absolute() or path.name in {"", ".", ".."}:
        raise ValueError("usage directory must be an absolute leaf path")

    exists = True
    try:
        metadata = os.lstat(path)
    except FileNotFoundError:
        exists = False
    else:
        if stat.S_ISLNK(metadata.st_mode):
            raise RuntimeError("usage directory must not be a symlink")
        if not stat.S_ISDIR(metadata.st_mode):
            raise RuntimeError("usage directory path exists but is not a directory")

    if not exists:
        try:
            os.mkdir(path, 0o700)
        except FileExistsError:
            # Another actor populated the path after validation. The O_NOFOLLOW open
            # below decides whether it is still safe without changing what appeared.
            pass

    flags = os.O_RDONLY | os.O_DIRECTORY
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        directory_fd = os.open(path, flags)
    except OSError as exc:
        raise RuntimeError("usage directory could not be opened without following symlinks") from exc
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
