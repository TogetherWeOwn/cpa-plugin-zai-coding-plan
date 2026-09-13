// Package securestore provides directory-confined, symlink-resistant JSON
// file storage: openat/O_NOFOLLOW-style access via a cached directory
// handle, 0700 directory / 0600 file permissions, atomic temp-file-then-rename
// writes with directory fsync, and os.SameFile-based directory-replacement
// detection. Callers compose the full storage directory path themselves;
// this package performs no path composition of its own.
package securestore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const maxFileSize = 8 << 20

// WriteOutcome classifies whether a write reached a durable, recoverable state.
type WriteOutcome uint8

const (
	WriteNotCommitted WriteOutcome = iota
	WriteCommitted
	WriteNeedsRecovery
)

// WriteError reports the outcome of a write that failed after the point at
// which its effect may already be durable (e.g. rename succeeded but the
// subsequent directory fsync did not).
type WriteError struct {
	Outcome WriteOutcome
	Err     error
}

func (e *WriteError) Error() string { return e.Err.Error() }
func (e *WriteError) Unwrap() error { return e.Err }

// OutcomeOf extracts the WriteOutcome carried by err, or WriteNotCommitted
// if err does not wrap a *WriteError.
func OutcomeOf(err error) WriteOutcome {
	var writeErr *WriteError
	if errors.As(err, &writeErr) {
		return writeErr.Outcome
	}
	return WriteNotCommitted
}

// Store is a directory-confined secure JSON file store.
type Store struct {
	mu        sync.RWMutex
	dir       string
	dirHandle *os.File
	dirSync   func(*os.File) error
	fileOpen  func(*os.File, string) (*os.File, error)
	closed    bool
}

// New opens (creating if necessary) a secure storage directory at dir. dir
// must already be the full, absolute path the caller wants used on disk;
// New performs no joining, prefixing, or namespacing of its own.
func New(dir string) (*Store, error) {
	clean := filepath.Clean(strings.TrimSpace(dir))
	if clean == "." || !filepath.IsAbs(clean) {
		return nil, fmt.Errorf("secure store dir must be an absolute path")
	}
	if err := ensureSecureDirectory(clean); err != nil {
		return nil, err
	}
	dirHandle, err := openSecureDirectory(clean)
	if err != nil {
		return nil, err
	}
	return &Store{dir: clean, dirHandle: dirHandle}, nil
}

// EnsureEmptyProviderDir creates (if necessary) a secure, symlink-rejecting
// directory at dir without opening or populating it. It is intended for
// reserving a provider's storage location before that provider has any
// state to persist.
func EnsureEmptyProviderDir(dir string) error {
	clean := filepath.Clean(strings.TrimSpace(dir))
	if clean == "." || !filepath.IsAbs(clean) {
		return fmt.Errorf("secure store dir must be an absolute path")
	}
	return ensureSecureDirectory(clean)
}

func (s *Store) ReadJSON(name string, dst any) error {
	_, err := s.ReadJSONIfExists(name, dst)
	return err
}

func (s *Store) ReadJSONIfExists(name string, dst any) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, err := s.securePathLocked(name); err != nil {
		return false, err
	}
	if err := s.validateDirectoryLocked(); err != nil {
		return false, err
	}
	file, err := s.openFileLocked(name)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open %s: %w", name, err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return false, fmt.Errorf("stat %s: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("%s is not a regular file", name)
	}
	if info.Mode().Perm() != 0o600 {
		return false, fmt.Errorf("%s has insecure permissions", name)
	}
	if info.Size() > maxFileSize {
		return false, fmt.Errorf("%s exceeds maximum size", name)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxFileSize+1))
	if err != nil {
		return false, fmt.Errorf("read %s: %w", name, err)
	}
	if len(data) == 0 {
		return false, fmt.Errorf("decode %s: empty file", name)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if errDecode := decoder.Decode(dst); errDecode != nil {
		return false, fmt.Errorf("decode %s: corrupt state", name)
	}
	if errDecode := EnsureJSONEOF(decoder); errDecode != nil {
		return false, fmt.Errorf("decode %s: corrupt state", name)
	}
	return true, nil
}

func (s *Store) WriteJSON(name string, value any) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, err := s.securePathLocked(name); err != nil {
		return err
	}
	if err := s.validateDirectoryLocked(); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode %s: %w", name, err)
	}
	data = append(data, '\n')
	if len(data) > maxFileSize {
		return fmt.Errorf("%s exceeds maximum size", name)
	}
	if err := writeJSONAt(s.dirHandle, name, data, s.syncDirLocked); err != nil {
		return err
	}
	return s.validateDirectoryLocked()
}

func (s *Store) RemoveJSON(name string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, err := s.securePathLocked(name); err != nil {
		return err
	}
	if err := s.validateDirectoryLocked(); err != nil {
		return err
	}
	if err := removeJSONAt(s.dirHandle, name, s.syncDirLocked); err != nil {
		return err
	}
	return s.validateDirectoryLocked()
}

func (s *Store) securePathLocked(name string) (string, error) {
	if s == nil || s.dir == "" || s.closed {
		return "", fmt.Errorf("secure store is not initialized")
	}
	if filepath.Base(name) != name || name == "." || name == "" {
		return "", fmt.Errorf("invalid store file name")
	}
	return filepath.Join(s.dir, name), nil
}

// Dir returns the absolute directory path this store was opened against.
func (s *Store) Dir() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dir
}

// SetDirSyncForTest overrides the directory-fsync hook used after writes and
// removals, for fault-injection tests. Not for production use. Deliberately
// unsynchronized (matching the field's own access pattern) so tests may
// reassign the hook reentrantly from within a call it is driving.
func (s *Store) SetDirSyncForTest(fn func(*os.File) error) {
	s.dirSync = fn
}

// SetFileOpenForTest overrides the descriptor-relative file-open hook used
// for reads, for fault-injection tests. Not for production use.
func (s *Store) SetFileOpenForTest(fn func(*os.File, string) (*os.File, error)) {
	s.fileOpen = fn
}

// OpenFileAt exposes the package's descriptor-relative open for tests that
// need to fall through to default behavior after simulating a fault.
func OpenFileAt(dir *os.File, name string) (*os.File, error) {
	return openFileAt(dir, name)
}

func (s *Store) ValidateDirectory() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.validateDirectoryLocked()
}

func (s *Store) validateDirectoryLocked() error {
	if s == nil || s.dir == "" || s.dirHandle == nil || s.closed {
		return fmt.Errorf("secure store is not initialized")
	}
	pathInfo, err := os.Lstat(s.dir)
	if err != nil {
		return fmt.Errorf("inspect secure store directory: %w", err)
	}
	handleInfo, err := s.dirHandle.Stat()
	if err != nil {
		return fmt.Errorf("inspect opened secure store directory: %w", err)
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.IsDir() || !handleInfo.IsDir() {
		return fmt.Errorf("secure store path is not a directory")
	}
	if pathInfo.Mode().Perm() != 0o700 || handleInfo.Mode().Perm() != 0o700 {
		return fmt.Errorf("secure store directory has insecure permissions")
	}
	if !os.SameFile(pathInfo, handleInfo) {
		return fmt.Errorf("secure store directory was replaced")
	}
	if err := RejectSymlinkPathComponents(filepath.Dir(s.dir)); err != nil {
		return err
	}
	return nil
}

func (s *Store) openFileLocked(name string) (*os.File, error) {
	if s.fileOpen != nil {
		return s.fileOpen(s.dirHandle, name)
	}
	return openFileAt(s.dirHandle, name)
}

func (s *Store) syncDirLocked(dir *os.File) error {
	if s.dirSync != nil {
		return s.dirSync(dir)
	}
	return dir.Sync()
}

func (s *Store) Flush() error {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.validateDirectoryLocked(); err != nil {
		return err
	}
	return s.syncDirLocked(s.dirHandle)
}

func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	handle := s.dirHandle
	s.dirHandle = nil
	if handle == nil {
		return nil
	}
	return handle.Close()
}

func ensureSecureDirectory(dir string) error {
	if err := RejectSymlinkPathComponents(filepath.Dir(dir)); err != nil {
		return err
	}
	if info, err := os.Lstat(dir); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("secure store path is not a directory")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect secure store directory: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create secure store directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("chmod secure store directory: %w", err)
	}
	return nil
}

// EnsureJSONEOF confirms decoder has no further JSON values buffered after
// the value already decoded from it.
func EnsureJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return fmt.Errorf("multiple JSON values")
	}
	return err
}

// RejectSymlinkPathComponents walks path's ancestry (from the first existing
// component down to root) and fails if any resolved component differs from
// its unresolved form, i.e. if a symlink is present anywhere in the chain.
func RejectSymlinkPathComponents(path string) error {
	clean := filepath.Clean(path)
	for {
		if _, err := os.Lstat(clean); err == nil {
			resolved, errEval := filepath.EvalSymlinks(clean)
			if errEval != nil {
				return fmt.Errorf("inspect secure store parent: %w", errEval)
			}
			if resolved != clean {
				return fmt.Errorf("secure store parent contains a symlink")
			}
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect secure store parent: %w", err)
		}
		parent := filepath.Dir(clean)
		if parent == clean {
			return nil
		}
		clean = parent
	}
}
