package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const maxStateFileSize = 8 << 20

type settingsFile struct {
	Version  int                       `json:"version"`
	Accounts map[string]accountSetting `json:"accounts,omitempty"`
}

type accountSetting struct {
	Name            string `json:"name,omitempty"`
	Plan            string `json:"plan,omitempty"`
	Disabled        *bool  `json:"disabled,omitempty"`
	FiveHourCredits int64  `json:"five_hour_credits,omitempty"`
	WeeklyCredits   int64  `json:"weekly_credits,omitempty"`
}

type persistedState struct {
	Version    int                          `json:"version"`
	Accounts   map[string]accountQuotaState `json:"accounts,omitempty"`
	Generation uint64                       `json:"-"`
}

type secureStore struct {
	dir       string
	dirHandle *os.File
}

func newSecureStore(authDir string) (*secureStore, error) {
	base := filepath.Clean(strings.TrimSpace(authDir))
	if base == "." || !filepath.IsAbs(base) {
		return nil, fmt.Errorf("auth-dir must be an absolute path")
	}
	dir := filepath.Join(base, pluginID)
	if err := ensureSecureDirectory(dir); err != nil {
		return nil, err
	}
	dirHandle, err := openSecureDirectory(dir)
	if err != nil {
		return nil, err
	}
	return &secureStore{dir: dir, dirHandle: dirHandle}, nil
}

func (s *secureStore) loadSettings() (settingsFile, error) {
	var settings settingsFile
	if err := s.readJSON("settings.json", &settings); err != nil {
		return settingsFile{}, err
	}
	return settings, nil
}

func (s *secureStore) saveSettings(settings settingsFile) error {
	return s.writeJSON("settings.json", settings)
}

func (s *secureStore) loadState() (persistedState, error) {
	var state persistedState
	if err := s.readJSON("state.json", &state); err != nil {
		return persistedState{}, err
	}
	if err := validatePersistedState(state); err != nil {
		return persistedState{}, err
	}
	return state, nil
}

func (s *secureStore) saveState(state persistedState) error {
	if err := validatePersistedState(state); err != nil {
		return err
	}
	return s.writeJSON("state.json", state)
}

func validatePersistedState(state persistedState) error {
	if state.Version == 0 && len(state.Accounts) == 0 {
		return nil
	}
	if state.Version != 1 {
		return fmt.Errorf("state.json has unsupported version")
	}
	for identity, accountState := range state.Accounts {
		if len(identity) != sha256.Size*2 {
			return fmt.Errorf("state.json contains invalid account identity")
		}
		if _, err := hex.DecodeString(identity); err != nil {
			return fmt.Errorf("state.json contains invalid account identity")
		}
		if err := validateAccountQuotaState(accountState); err != nil {
			return fmt.Errorf("state.json contains invalid account state")
		}
	}
	return nil
}

func validateAccountQuotaState(state accountQuotaState) error {
	if state.ConsecutiveFailures < 0 || len(state.DedupHashes) > maxDedupHashes {
		return fmt.Errorf("invalid polling metadata")
	}
	if state.Authoritative != nil {
		for _, window := range []quotaWindow{state.Authoritative.FiveHour, state.Authoritative.Weekly} {
			if window.ConsumedMicrocredits < 0 || window.BucketMicrocredits <= 0 || window.ConsumedMicrocredits > window.BucketMicrocredits || window.ResetsAt.IsZero() {
				return fmt.Errorf("invalid authoritative quota")
			}
		}
	}
	last := time.Time{}
	for _, event := range state.Events {
		if event.At.IsZero() || event.Microcredits <= 0 || normalizeModelName(event.Model) == "" || (!last.IsZero() && event.At.Before(last)) {
			return fmt.Errorf("invalid credit event")
		}
		last = event.At
	}
	for _, hash := range state.DedupHashes {
		if len(hash) != sha256.Size*2 {
			return fmt.Errorf("invalid dedup hash")
		}
		if _, err := hex.DecodeString(hash); err != nil {
			return fmt.Errorf("invalid dedup hash")
		}
	}
	return nil
}

func applyStoredSettings(accounts []account, settings settingsFile) error {
	if settings.Version == 0 && len(settings.Accounts) == 0 {
		return nil
	}
	if settings.Version != 1 {
		return fmt.Errorf("settings.json has unsupported version")
	}
	for identity, stored := range settings.Accounts {
		if len(identity) != sha256.Size*2 {
			return fmt.Errorf("settings.json contains invalid account identity")
		}
		if _, err := hex.DecodeString(identity); err != nil {
			return fmt.Errorf("settings.json contains invalid account identity")
		}
		if err := validateStoredSetting(stored); err != nil {
			return fmt.Errorf("settings.json contains invalid account settings")
		}
	}
	for i := range accounts {
		stored, ok := settings.Accounts[accounts[i].Identity]
		if !ok {
			continue
		}
		if name := strings.TrimSpace(stored.Name); name != "" {
			accounts[i].Name = name
		}
		if plan := normalizePlan(stored.Plan); plan != "" {
			accounts[i].Plan = plan
			if buckets, known := planBuckets[plan]; known {
				accounts[i].FiveHourCredits = buckets.FiveHour
				accounts[i].WeeklyCredits = buckets.Weekly
			}
		}
		if stored.Disabled != nil {
			accounts[i].Disabled = *stored.Disabled
		}
		if stored.FiveHourCredits > 0 {
			accounts[i].FiveHourCredits = stored.FiveHourCredits
		}
		if stored.WeeklyCredits > 0 {
			accounts[i].WeeklyCredits = stored.WeeklyCredits
		}
	}
	return validateAccounts(accounts)
}

func validateStoredSetting(stored accountSetting) error {
	plan := normalizePlan(stored.Plan)
	if stored.Plan != "" && plan == "" {
		return fmt.Errorf("invalid plan")
	}
	if stored.FiveHourCredits < 0 || stored.WeeklyCredits < 0 {
		return fmt.Errorf("negative credit bucket")
	}
	if plan == "custom" && (stored.FiveHourCredits <= 0 || stored.WeeklyCredits <= 0) {
		return fmt.Errorf("custom plan requires both credit buckets")
	}
	return nil
}

func validateAccounts(accounts []account) error {
	seenNames := make(map[string]struct{}, len(accounts))
	for i := range accounts {
		if strings.TrimSpace(accounts[i].Name) == "" {
			return fmt.Errorf("account has no name")
		}
		if normalizePlan(accounts[i].Plan) == "" {
			return fmt.Errorf("account has invalid plan")
		}
		if accounts[i].FiveHourCredits <= 0 || accounts[i].WeeklyCredits <= 0 {
			return fmt.Errorf("account has non-positive credit bucket")
		}
		nameKey := strings.ToLower(strings.TrimSpace(accounts[i].Name))
		if _, exists := seenNames[nameKey]; exists {
			return fmt.Errorf("duplicate account name")
		}
		seenNames[nameKey] = struct{}{}
	}
	return nil
}

func (s *secureStore) readJSON(name string, dst any) error {
	if err := s.validateDirectory(); err != nil {
		return err
	}
	path, err := s.securePath(name)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscallNoFollow, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", name, err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", name)
	}
	if info.Mode().Perm() != 0o600 {
		return fmt.Errorf("%s has insecure permissions", name)
	}
	if info.Size() > maxStateFileSize {
		return fmt.Errorf("%s exceeds maximum size", name)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxStateFileSize+1))
	if err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}
	if len(data) == 0 {
		return fmt.Errorf("decode %s: empty file", name)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if errDecode := decoder.Decode(dst); errDecode != nil {
		return fmt.Errorf("decode %s: corrupt state", name)
	}
	if errDecode := ensureJSONEOF(decoder); errDecode != nil {
		return fmt.Errorf("decode %s: corrupt state", name)
	}
	return nil
}

func (s *secureStore) writeJSON(name string, value any) error {
	if _, err := s.securePath(name); err != nil {
		return err
	}
	if err := s.validateDirectory(); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode %s: %w", name, err)
	}
	data = append(data, '\n')
	if len(data) > maxStateFileSize {
		return fmt.Errorf("%s exceeds maximum size", name)
	}
	if err := writeJSONAt(s.dirHandle, name, data); err != nil {
		return err
	}
	return s.validateDirectory()
}

func (s *secureStore) securePath(name string) (string, error) {
	if s == nil || s.dir == "" {
		return "", fmt.Errorf("secure store is not initialized")
	}
	if filepath.Base(name) != name || name == "." || name == "" {
		return "", fmt.Errorf("invalid store file name")
	}
	return filepath.Join(s.dir, name), nil
}

func (s *secureStore) validateDirectory() error {
	if s == nil || s.dir == "" || s.dirHandle == nil {
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
	if err := rejectSymlinkPathComponents(filepath.Dir(s.dir)); err != nil {
		return err
	}
	return nil
}

func (s *secureStore) flush() error {
	if s == nil {
		return nil
	}
	if err := s.validateDirectory(); err != nil {
		return err
	}
	return s.dirHandle.Sync()
}

func ensureSecureDirectory(dir string) error {
	if err := rejectSymlinkPathComponents(filepath.Dir(dir)); err != nil {
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

func ensureJSONEOF(decoder *json.Decoder) error {
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

func rejectSymlinkPathComponents(path string) error {
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

func rejectExistingSymlink(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect target: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing symlink target")
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("target is not a regular file")
	}
	return nil
}

func syncDirectory(dir string) error {
	file, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open secure store directory: %w", err)
	}
	defer func() { _ = file.Close() }()
	if errSync := file.Sync(); errSync != nil {
		return fmt.Errorf("sync secure store directory: %w", errSync)
	}
	return nil
}
