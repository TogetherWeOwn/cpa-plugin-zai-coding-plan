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
	"sync"
	"time"
)

const (
	maxStateFileSize            = 8 << 20
	maxPersistedClockSkew       = 5 * time.Minute
	settingsRecoveryVersion     = 1
	persistedStateVersion       = 2
	legacyPersistedStateVersion = 1
)

type writeOutcome uint8

const (
	writeNotCommitted writeOutcome = iota
	writeCommitted
	writeNeedsRecovery
)

type storeWriteError struct {
	outcome writeOutcome
	err     error
}

func (e *storeWriteError) Error() string { return e.err.Error() }
func (e *storeWriteError) Unwrap() error { return e.err }

type settingsRecoveryPendingError struct {
	err error
}

func (e *settingsRecoveryPendingError) Error() string { return e.err.Error() }
func (e *settingsRecoveryPendingError) Unwrap() error { return e.err }

func settingsRecoveryPending(err error) bool {
	var pending *settingsRecoveryPendingError
	return errors.As(err, &pending)
}

func writeErrorOutcome(err error) writeOutcome {
	var writeErr *storeWriteError
	if errors.As(err, &writeErr) {
		return writeErr.outcome
	}
	return writeNotCommitted
}

type settingsFile struct {
	Version             int                       `json:"version"`
	Accounts            map[string]accountSetting `json:"accounts,omitempty"`
	ThresholdPercent    *int                      `json:"threshold_percent,omitempty"`
	PollingInterval     string                    `json:"polling_interval,omitempty"`
	AuthoritativeMaxAge string                    `json:"authoritative_max_age,omitempty"`
	Timeout             string                    `json:"timeout,omitempty"`
}

type accountSetting struct {
	Name            string `json:"name,omitempty"`
	Plan            string `json:"plan,omitempty"`
	Disabled        *bool  `json:"disabled,omitempty"`
	FiveHourCredits int64  `json:"five_hour_credits,omitempty"`
	WeeklyCredits   int64  `json:"weekly_credits,omitempty"`
}

type persistedState struct {
	Version           int                             `json:"version"`
	Accounts          map[string]accountQuotaState    `json:"accounts,omitempty"`
	Health            map[string]persistedHealthState `json:"health,omitempty"`
	Generation        uint64                          `json:"-"`
	RuntimeGeneration uint64                          `json:"-"`
}

type persistedHealthState struct {
	SuspendedUntil  time.Time `json:"suspended_until,omitempty"`
	ExhaustedUntil  time.Time `json:"exhausted_until,omitempty"`
	ExhaustedReason string    `json:"exhausted_reason,omitempty"`
}

type settingsRecovery struct {
	Version        int           `json:"version"`
	PreviousDigest string        `json:"previous_digest"`
	Desired        *settingsFile `json:"desired,omitempty"`
}

type secureStore struct {
	mu        sync.RWMutex
	dir       string
	dirHandle *os.File
	dirSync   func(*os.File) error
	fileOpen  func(*os.File, string) (*os.File, error)
	closed    bool
}

const settingsRecoveryName = "settings.recovery.json"

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
	if err := validateSettingsFile(settings); err != nil {
		return settingsFile{}, err
	}
	return settings, nil
}

func validateSettingsFile(settings settingsFile) error {
	if settings.Version == 0 && settingsEmpty(settings) {
		return nil
	}
	if settings.Version != 1 {
		return fmt.Errorf("settings.json has unsupported version")
	}
	if _, err := applyStoredConfig(pluginConfig{
		QuotaRefresh:        defaultQuotaRefresh,
		AuthoritativeMaxAge: defaultAuthoritativeMaxAge,
		QuotaTimeout:        defaultQuotaTimeout,
		ThresholdPercent:    defaultThreshold,
	}, settings); err != nil {
		return err
	}
	return nil
}

func (s *secureStore) saveSettings(settings settingsFile) error {
	previous, err := s.loadSettings()
	if err != nil {
		if strings.Contains(err.Error(), "too many levels of symbolic links") {
			return fmt.Errorf("refusing symlink target")
		}
		return err
	}
	recovery := settingsRecovery{
		Version:        settingsRecoveryVersion,
		PreviousDigest: settingsDigest(previous),
	}
	if err := s.writeJSON(settingsRecoveryName, recovery); err != nil {
		return fmt.Errorf("stage settings recovery: %w", err)
	}
	recovery.Desired = &settings
	if err := s.writeJSON(settingsRecoveryName, recovery); err != nil {
		if writeErrorOutcome(err) != writeNeedsRecovery {
			return fmt.Errorf("prepare settings recovery: %w", err)
		}
		if flushErr := s.flush(); flushErr != nil {
			current, readErr := s.loadSettings()
			if readErr == nil && settingsDigest(current) == settingsDigest(settings) {
				return nil
			}
			return &settingsRecoveryPendingError{err: fmt.Errorf("prepare settings recovery: %w", err)}
		}
	}
	if err := s.writeJSON("settings.json", settings); err != nil {
		if writeErrorOutcome(err) == writeNeedsRecovery {
			if flushErr := s.flush(); flushErr == nil {
				return nil
			}
		}
		return fmt.Errorf("commit settings: %w", err)
	}
	if err := s.removeJSON(settingsRecoveryName); err != nil {
		if writeErrorOutcome(err) != writeNeedsRecovery {
			return fmt.Errorf("clear settings recovery: %w", err)
		}
	}
	return nil
}

func (s *secureStore) recoverSettings() (settingsFile, error) {
	settings, err := s.loadSettings()
	if err != nil {
		return settingsFile{}, err
	}
	var recovery settingsRecovery
	found, err := s.readJSONIfExists(settingsRecoveryName, &recovery)
	if err != nil {
		return settingsFile{}, err
	}
	if !found {
		return settings, nil
	}
	if recovery.Version != settingsRecoveryVersion {
		return settingsFile{}, fmt.Errorf("settings recovery marker is invalid")
	}
	currentDigest := settingsDigest(settings)
	if recovery.Desired == nil {
		if currentDigest != recovery.PreviousDigest {
			return settingsFile{}, fmt.Errorf("settings recovery marker does not match settings.json")
		}
		_ = s.removeJSON(settingsRecoveryName)
		return settings, nil
	}
	if validateSettingsFile(*recovery.Desired) != nil {
		return settingsFile{}, fmt.Errorf("settings recovery marker is invalid")
	}
	desiredDigest := settingsDigest(*recovery.Desired)
	switch currentDigest {
	case recovery.PreviousDigest:
		if err := s.writeJSON("settings.json", *recovery.Desired); err != nil && writeErrorOutcome(err) != writeNeedsRecovery {
			return settingsFile{}, fmt.Errorf("roll forward settings recovery: %w", err)
		}
		_ = s.removeJSON(settingsRecoveryName)
		return *recovery.Desired, nil
	case desiredDigest:
		_ = s.removeJSON(settingsRecoveryName)
		return *recovery.Desired, nil
	default:
		return settingsFile{}, fmt.Errorf("settings recovery marker does not match settings.json")
	}
}

func settingsDigest(settings settingsFile) string {
	raw, _ := json.Marshal(settings)
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func (s *secureStore) loadState() (persistedState, error) {
	return s.loadStateAt(time.Now().UTC())
}

func (s *secureStore) loadStateAt(now time.Time) (persistedState, error) {
	var state persistedState
	if err := s.readJSON("state.json", &state); err != nil {
		return persistedState{}, err
	}
	if err := validatePersistedStateAt(state, now); err != nil {
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
	return validatePersistedStateAt(state, time.Now().UTC())
}

func validatePersistedStateAt(state persistedState, now time.Time) error {
	if state.Version == 0 && len(state.Accounts) == 0 && len(state.Health) == 0 {
		return nil
	}
	if state.Version != legacyPersistedStateVersion && state.Version != persistedStateVersion {
		return fmt.Errorf("state.json has unsupported version")
	}
	if state.Version == legacyPersistedStateVersion && len(state.Health) != 0 {
		return fmt.Errorf("state.json has unsupported health data")
	}
	for identity, accountState := range state.Accounts {
		if err := validateAccountIdentity(identity); err != nil {
			return err
		}
		if err := validateAccountQuotaState(accountState, now); err != nil {
			return fmt.Errorf("state.json contains invalid account state")
		}
	}
	for identity, healthState := range state.Health {
		if err := validateAccountIdentity(identity); err != nil {
			return err
		}
		if err := validatePersistedHealthState(healthState); err != nil {
			return fmt.Errorf("state.json contains invalid health state")
		}
	}
	return nil
}

func validateAccountIdentity(identity string) error {
	if len(identity) != sha256.Size*2 {
		return fmt.Errorf("state.json contains invalid account identity")
	}
	if _, err := hex.DecodeString(identity); err != nil {
		return fmt.Errorf("state.json contains invalid account identity")
	}
	return nil
}

func validatePersistedHealthState(state persistedHealthState) error {
	const maxPersistedHealthYear = 9999
	if !state.SuspendedUntil.IsZero() && (state.SuspendedUntil.Year() < 2000 || state.SuspendedUntil.Year() > maxPersistedHealthYear) {
		return fmt.Errorf("invalid suspension time")
	}
	if !state.ExhaustedUntil.IsZero() && (state.ExhaustedUntil.Year() < 2000 || state.ExhaustedUntil.Year() > maxPersistedHealthYear) {
		return fmt.Errorf("invalid exhaustion time")
	}
	if !state.ExhaustedUntil.IsZero() && strings.TrimSpace(state.ExhaustedReason) == "" {
		return fmt.Errorf("missing exhaustion reason")
	}
	if len(state.ExhaustedReason) > 96 {
		return fmt.Errorf("invalid exhaustion reason")
	}
	return nil
}

func validateAccountQuotaState(state accountQuotaState, now time.Time) error {
	futureLimit := now.UTC().Add(maxPersistedClockSkew)
	if state.ConsecutiveFailures < 0 || len(state.DedupHashes) > maxDedupHashes {
		return fmt.Errorf("invalid polling metadata")
	}
	if state.Authoritative != nil {
		if state.Authoritative.ObservedAt.IsZero() || state.Authoritative.ObservedAt.Year() < 2000 || state.Authoritative.ObservedAt.After(futureLimit) {
			return fmt.Errorf("invalid authoritative observation time")
		}
		for _, window := range []quotaWindow{state.Authoritative.FiveHour, state.Authoritative.Weekly} {
			if window.ConsumedMicrocredits < 0 || window.BucketMicrocredits <= 0 || window.ConsumedMicrocredits > window.BucketMicrocredits || window.ResetsAt.IsZero() {
				return fmt.Errorf("invalid authoritative quota")
			}
		}
	}
	last := time.Time{}
	for _, event := range state.Events {
		if event.At.IsZero() || event.At.After(futureLimit) || event.Microcredits <= 0 || normalizeModelName(event.Model) == "" || (!last.IsZero() && event.At.Before(last)) {
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
	if settings.Version == 0 && settingsEmpty(settings) {
		return nil
	}
	if settings.Version != 1 {
		return fmt.Errorf("settings.json has unsupported version")
	}
	if _, err := applyStoredConfig(pluginConfig{
		QuotaRefresh:        defaultQuotaRefresh,
		AuthoritativeMaxAge: defaultAuthoritativeMaxAge,
		QuotaTimeout:        defaultQuotaTimeout,
		ThresholdPercent:    defaultThreshold,
	}, settings); err != nil {
		return err
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
			accounts[i].planExplicit = true
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
			accounts[i].fiveHourCreditsExplicit = true
		}
		if stored.WeeklyCredits > 0 {
			accounts[i].WeeklyCredits = stored.WeeklyCredits
			accounts[i].weeklyCreditsExplicit = true
		}
	}
	return validateAccounts(accounts)
}

func settingsEmpty(settings settingsFile) bool {
	return len(settings.Accounts) == 0 && settings.ThresholdPercent == nil && settings.PollingInterval == "" && settings.AuthoritativeMaxAge == "" && settings.Timeout == ""
}

func applyStoredConfig(cfg pluginConfig, settings settingsFile) (pluginConfig, error) {
	if settings.ThresholdPercent != nil {
		if *settings.ThresholdPercent < 1 || *settings.ThresholdPercent > 100 {
			return pluginConfig{}, fmt.Errorf("settings.json contains invalid threshold_percent")
		}
		cfg.ThresholdPercent = *settings.ThresholdPercent
	}
	var err error
	if settings.PollingInterval != "" {
		cfg.QuotaRefresh, err = parsePositiveDuration("polling_interval", settings.PollingInterval, cfg.QuotaRefresh)
		if err != nil || cfg.QuotaRefresh < time.Minute || cfg.QuotaRefresh > 3*time.Minute {
			return pluginConfig{}, fmt.Errorf("settings.json contains invalid polling_interval")
		}
	}
	if settings.AuthoritativeMaxAge != "" {
		cfg.AuthoritativeMaxAge, err = parsePositiveDuration("authoritative_max_age", settings.AuthoritativeMaxAge, cfg.AuthoritativeMaxAge)
		if err != nil {
			return pluginConfig{}, fmt.Errorf("settings.json contains invalid authoritative_max_age")
		}
	}
	if cfg.AuthoritativeMaxAge <= maxPollInterval(cfg.QuotaRefresh) {
		return pluginConfig{}, fmt.Errorf("settings.json authoritative_max_age must exceed maximum polling jitter")
	}
	if settings.Timeout != "" {
		cfg.QuotaTimeout, err = parsePositiveDuration("timeout", settings.Timeout, cfg.QuotaTimeout)
		if err != nil || cfg.QuotaTimeout < minQuotaTimeout || cfg.QuotaTimeout > maxQuotaTimeout {
			return pluginConfig{}, fmt.Errorf("settings.json contains invalid timeout")
		}
	}
	return cfg, nil
}

func validateStoredSetting(stored accountSetting) error {
	plan := normalizePlan(stored.Plan)
	if stored.Plan != "" && plan == "" {
		return fmt.Errorf("invalid plan")
	}
	if stored.FiveHourCredits < 0 || stored.WeeklyCredits < 0 {
		return fmt.Errorf("negative credit bucket")
	}
	if stored.FiveHourCredits > 0 && !validCreditBucket(stored.FiveHourCredits) || stored.WeeklyCredits > 0 && !validCreditBucket(stored.WeeklyCredits) {
		return fmt.Errorf("credit bucket exceeds supported range")
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
		if !validCreditBucket(accounts[i].FiveHourCredits) || !validCreditBucket(accounts[i].WeeklyCredits) {
			return fmt.Errorf("account has non-positive or out-of-range credit bucket")
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
	_, err := s.readJSONIfExists(name, dst)
	return err
}

func (s *secureStore) readJSONIfExists(name string, dst any) (bool, error) {
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
	if info.Size() > maxStateFileSize {
		return false, fmt.Errorf("%s exceeds maximum size", name)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxStateFileSize+1))
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
	if errDecode := ensureJSONEOF(decoder); errDecode != nil {
		return false, fmt.Errorf("decode %s: corrupt state", name)
	}
	return true, nil
}

func (s *secureStore) writeJSON(name string, value any) error {
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
	if len(data) > maxStateFileSize {
		return fmt.Errorf("%s exceeds maximum size", name)
	}
	if err := writeJSONAt(s.dirHandle, name, data, s.syncDirLocked); err != nil {
		return err
	}
	return s.validateDirectoryLocked()
}

func (s *secureStore) removeJSON(name string) error {
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

func (s *secureStore) securePathLocked(name string) (string, error) {
	if s == nil || s.dir == "" || s.closed {
		return "", fmt.Errorf("secure store is not initialized")
	}
	if filepath.Base(name) != name || name == "." || name == "" {
		return "", fmt.Errorf("invalid store file name")
	}
	return filepath.Join(s.dir, name), nil
}

func (s *secureStore) validateDirectory() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.validateDirectoryLocked()
}

func (s *secureStore) validateDirectoryLocked() error {
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
	if err := rejectSymlinkPathComponents(filepath.Dir(s.dir)); err != nil {
		return err
	}
	return nil
}

func (s *secureStore) openFileLocked(name string) (*os.File, error) {
	if s.fileOpen != nil {
		return s.fileOpen(s.dirHandle, name)
	}
	return openFileAt(s.dirHandle, name)
}

func (s *secureStore) syncDirLocked(dir *os.File) error {
	if s.dirSync != nil {
		return s.dirSync(dir)
	}
	return dir.Sync()
}

func (s *secureStore) flush() error {
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

func (s *secureStore) close() error {
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
