//go:build linux

package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

const syscallNoFollow = unix.O_NOFOLLOW

func openSecureDirectory(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open secure store directory: %w", err)
	}
	return os.NewFile(uintptr(fd), path), nil
}

func writeJSONAt(dir *os.File, name string, data []byte, syncDir func(*os.File) error) error {
	if dir == nil {
		return fmt.Errorf("secure store is not initialized")
	}
	dirFD := int(dir.Fd())
	if err := rejectExistingTargetAt(dirFD, name); err != nil {
		return err
	}

	tempName, temp, err := createTempAt(dirFD, name)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		_ = temp.Close()
		if !committed {
			_ = unix.Unlinkat(dirFD, tempName, 0)
		}
	}()

	if _, errWrite := temp.Write(data); errWrite != nil {
		return fmt.Errorf("write temporary %s: %w", name, errWrite)
	}
	if errSync := temp.Sync(); errSync != nil {
		return fmt.Errorf("sync temporary %s: %w", name, errSync)
	}
	if errClose := temp.Close(); errClose != nil {
		return fmt.Errorf("close temporary %s: %w", name, errClose)
	}
	if err := rejectExistingTargetAt(dirFD, name); err != nil {
		return err
	}
	if errRename := unix.Renameat(dirFD, tempName, dirFD, name); errRename != nil {
		return fmt.Errorf("replace %s: %w", name, errRename)
	}
	committed = true
	if syncDir == nil {
		syncDir = func(dir *os.File) error { return dir.Sync() }
	}
	if errSync := syncDir(dir); errSync != nil {
		return &storeWriteError{outcome: writeNeedsRecovery, err: fmt.Errorf("sync secure store directory: %w", errSync)}
	}
	return nil
}

func removeJSONAt(dir *os.File, name string, syncDir func(*os.File) error) error {
	if dir == nil {
		return fmt.Errorf("secure store is not initialized")
	}
	dirFD := int(dir.Fd())
	if err := unix.Unlinkat(dirFD, name, 0); errors.Is(err, unix.ENOENT) {
		return nil
	} else if err != nil {
		return fmt.Errorf("remove %s: %w", name, err)
	}
	if syncDir == nil {
		syncDir = func(dir *os.File) error { return dir.Sync() }
	}
	if err := syncDir(dir); err != nil {
		return &storeWriteError{outcome: writeNeedsRecovery, err: fmt.Errorf("sync secure store directory: %w", err)}
	}
	return nil
}

func createTempAt(dirFD int, name string) (string, *os.File, error) {
	var suffix [12]byte
	for attempt := 0; attempt < 100; attempt++ {
		if _, err := rand.Read(suffix[:]); err != nil {
			return "", nil, fmt.Errorf("generate temporary %s name: %w", name, err)
		}
		tempName := "." + name + ".tmp-" + hex.EncodeToString(suffix[:])
		fd, err := unix.Openat(dirFD, tempName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return "", nil, fmt.Errorf("create temporary %s: %w", name, err)
		}
		return tempName, os.NewFile(uintptr(fd), tempName), nil
	}
	return "", nil, fmt.Errorf("create temporary %s: name collision", name)
}

func rejectExistingTargetAt(dirFD int, name string) error {
	var stat unix.Stat_t
	err := unix.Fstatat(dirFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect target: %w", err)
	}
	if stat.Mode&unix.S_IFMT == unix.S_IFLNK {
		return fmt.Errorf("refusing symlink target")
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("target is not a regular file")
	}
	return nil
}
