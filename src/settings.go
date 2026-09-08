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
)

const maxStateFileSize = 8 << 20

type settingsFile struct {
	Version  int                       `json:"version"`
	Accounts map[string]accountSetting `json:"accounts,omitempty"`
}

type accountSetting struct {
	Name            string `json:"name,omitempty"`
	Plan            string `json:"plan,omitempty"`
	Disabled        bool   `json:"disabled,omitempty"`
	FiveHourCredits int64  `json:"five_hour_credits,omitempty"`
	WeeklyCredits   int64  `json:"weekly_credits,omitempty"`
}

type persistedState struct {
	Version  int                        `json:"version"`
	Accounts map[string]json.RawMessage `json:"accounts,omitempty"`
}

type secureStore struct {
	dir string
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
	return &secureStore{dir: dir}, nil
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
	return state, nil
}

func (s *secureStore) saveState(state persistedState) error {
	return s.writeJSON("state.json", state)
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
		accounts[i].Disabled = stored.Disabled
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
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", name)
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
	path, err := s.securePath(name)
	if err != nil {
		return err
	}
	if errCheck := rejectExistingSymlink(path); errCheck != nil {
		return errCheck
	}
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode %s: %w", name, err)
	}
	data = append(data, '\n')
	if len(data) > maxStateFileSize {
		return fmt.Errorf("%s exceeds maximum size", name)
	}

	temp, err := os.CreateTemp(s.dir, "."+name+".tmp-")
	if err != nil {
		return fmt.Errorf("create temporary %s: %w", name, err)
	}
	tempPath := temp.Name()
	committed := false
	defer func() {
		_ = temp.Close()
		if !committed {
			_ = os.Remove(tempPath)
		}
	}()

	if errChmod := temp.Chmod(0o600); errChmod != nil {
		return fmt.Errorf("chmod temporary %s: %w", name, errChmod)
	}
	if _, errWrite := temp.Write(data); errWrite != nil {
		return fmt.Errorf("write temporary %s: %w", name, errWrite)
	}
	if errSync := temp.Sync(); errSync != nil {
		return fmt.Errorf("sync temporary %s: %w", name, errSync)
	}
	if errClose := temp.Close(); errClose != nil {
		return fmt.Errorf("close temporary %s: %w", name, errClose)
	}
	if errCheck := rejectExistingSymlink(path); errCheck != nil {
		return errCheck
	}
	if errRename := os.Rename(tempPath, path); errRename != nil {
		return fmt.Errorf("replace %s: %w", name, errRename)
	}
	committed = true
	return syncDirectory(s.dir)
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
	defer file.Close()
	if errSync := file.Sync(); errSync != nil {
		return fmt.Errorf("sync secure store directory: %w", errSync)
	}
	return nil
}
