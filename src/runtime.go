package main

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

type runtimeSnapshot struct {
	Config   pluginConfig
	Accounts []account
	Store    *secureStore
}

type pluginRuntime struct {
	mu           sync.RWMutex
	snapshot     *runtimeSnapshot
	lastErr      error
	stopped      bool
	reconfigures sync.WaitGroup
}

func (r *pluginRuntime) reconfigure(rawConfig []byte) error {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return fmt.Errorf("plugin is shutting down")
	}
	r.reconfigures.Add(1)
	r.mu.Unlock()
	defer r.reconfigures.Done()

	cfg, err := parsePluginConfig(rawConfig)
	if err != nil {
		return r.recordError(err)
	}
	configPath, err := resolveCPAConfigPath(cfg.CPAConfigPath)
	if err != nil {
		return r.recordError(err)
	}
	cpa, err := loadCPAConfig(configPath)
	if err != nil {
		return r.recordError(err)
	}
	providerKeys := cpaProviderKeys(cpa)
	authDir, err := resolveAuthDir(cpa.AuthDir)
	if err != nil {
		return r.recordError(err, providerKeys...)
	}
	store, err := newSecureStore(authDir)
	if err != nil {
		return r.recordError(err, providerKeys...)
	}
	accounts, err := discoverAccounts(cpa, cfg)
	if err != nil {
		return r.recordError(err, providerKeys...)
	}
	settings, err := store.loadSettings()
	if err != nil {
		return r.recordError(err, providerKeys...)
	}
	if err = applyStoredSettings(accounts, settings); err != nil {
		return r.recordError(err, providerKeys...)
	}
	if _, err = store.loadState(); err != nil {
		return r.recordError(err, providerKeys...)
	}

	staged := &runtimeSnapshot{Config: cfg, Accounts: accounts, Store: store}
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return fmt.Errorf("plugin is shutting down")
	}
	r.snapshot = staged
	r.lastErr = nil
	r.mu.Unlock()
	return nil
}

func (r *pluginRuntime) recordError(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	redacted := redactError(err, secrets...)
	r.mu.Lock()
	if !r.stopped {
		r.lastErr = redacted
	}
	r.mu.Unlock()
	return redacted
}

func (r *pluginRuntime) current() (*runtimeSnapshot, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.snapshot == nil {
		if r.lastErr != nil {
			return nil, r.lastErr
		}
		return nil, fmt.Errorf("plugin is not configured")
	}
	copySnapshot := *r.snapshot
	copySnapshot.Accounts = append([]account(nil), r.snapshot.Accounts...)
	return &copySnapshot, nil
}

// hasSnapshot is called from the linux/cgo ABI boundary, which is excluded
// from the default non-CGO lint build.
//
//nolint:unused
func (r *pluginRuntime) hasSnapshot() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.snapshot != nil
}

func (r *pluginRuntime) validationStatus() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.lastErr == nil {
		return ""
	}
	return boundedStatus(r.lastErr.Error())
}

func (r *pluginRuntime) shutdown() error {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return nil
	}
	r.stopped = true
	r.mu.Unlock()
	r.reconfigures.Wait()

	r.mu.RLock()
	snapshot := r.snapshot
	r.mu.RUnlock()
	if snapshot == nil || snapshot.Store == nil {
		return nil
	}
	return snapshot.Store.flush()
}

func cpaProviderKeys(cpa cpaConfigProjection) []string {
	keys := make([]string, 0, len(cpa.ClaudeKeys)+len(cpa.OpenAICompatibility))
	for i := range cpa.ClaudeKeys {
		keys = append(keys, cpa.ClaudeKeys[i].APIKey)
	}
	for i := range cpa.OpenAICompatibility {
		for j := range cpa.OpenAICompatibility[i].APIKeyEntries {
			keys = append(keys, cpa.OpenAICompatibility[i].APIKeyEntries[j].APIKey)
		}
	}
	return keys
}

func redactError(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	for _, secret := range secrets {
		secret = strings.TrimSpace(secret)
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
	return errors.New(message)
}

func boundedStatus(message string) string {
	const maxStatusLength = 240
	clean := strings.Join(strings.Fields(message), " ")
	if len(clean) > maxStatusLength {
		clean = clean[:maxStatusLength]
	}
	return clean
}
