#!/usr/bin/env python3
"""Remove the fixed collector output through descriptor-relative no-follow operations."""
from __future__ import annotations

import os
import pathlib
import stat

TRUSTED_OUTPUT = pathlib.Path("/srv/cliproxy-usage/zai.json")


def remove_usage_output(
    path: pathlib.Path = TRUSTED_OUTPUT,
    *,
    expected_uid: int = 0,
    trusted_output: pathlib.Path = TRUSTED_OUTPUT,
    filesystem_root: pathlib.Path = pathlib.Path("/"),
) -> None:
    if not path.is_absolute() or path != trusted_output or trusted_output != TRUSTED_OUTPUT:
        raise ValueError(f"output path must be exactly {TRUSTED_OUTPUT}")
    if not filesystem_root.is_absolute():
        raise ValueError("filesystem root must be absolute")

    flags = os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0)
    try:
        directory_fd = os.open(filesystem_root, flags)
    except OSError as exc:
        raise RuntimeError("filesystem root could not be opened securely") from exc
    try:
        for component in path.parts[1:-1]:
            try:
                next_fd = os.open(component, flags, dir_fd=directory_fd)
            except OSError as exc:
                raise RuntimeError("usage directory path could not be opened securely") from exc
            os.close(directory_fd)
            directory_fd = next_fd

        metadata = os.fstat(directory_fd)
        if not stat.S_ISDIR(metadata.st_mode):
            raise RuntimeError("usage directory is not a directory")
        if metadata.st_uid != expected_uid or stat.S_IMODE(metadata.st_mode) != 0o700:
            raise RuntimeError("usage directory ownership or mode validation failed")
        try:
            output = os.stat(path.name, dir_fd=directory_fd, follow_symlinks=False)
        except FileNotFoundError:
            return
        except OSError as exc:
            raise RuntimeError("usage output could not be inspected securely") from exc
        if stat.S_ISLNK(output.st_mode) or not stat.S_ISREG(output.st_mode):
            raise RuntimeError("usage output must be a regular no-follow file")
        os.unlink(path.name, dir_fd=directory_fd)
        os.fsync(directory_fd)
    finally:
        os.close(directory_fd)


def main() -> int:
    remove_usage_output()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
