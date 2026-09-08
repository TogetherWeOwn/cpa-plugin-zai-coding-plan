package main

import (
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
	authDir, err := resolveAuthDir(cpa.AuthDir)
	if err != nil {
		return r.recordError(err)
	}
	store, err := newSecureStore(authDir)
	if err != nil {
		return r.recordError(err)
	}
	accounts, err := discoverAccounts(cpa, cfg)
	if err != nil {
		return r.recordError(err)
	}
	settings, err := store.loadSettings()
	if err != nil {
		return r.recordError(err)
	}
	if err = applyStoredSettings(accounts, settings); err != nil {
		return r.recordError(err)
	}
	if _, err = store.loadState(); err != nil {
		return r.recordError(err)
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

func (r *pluginRuntime) recordError(err error) error {
	if err == nil {
		return nil
	}
	r.mu.Lock()
	if !r.stopped {
		r.lastErr = err
	}
	r.mu.Unlock()
	return err
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

func boundedStatus(message string) string {
	const maxStatusLength = 240
	clean := strings.Join(strings.Fields(message), " ")
	if len(clean) > maxStatusLength {
		clean = clean[:maxStatusLength]
	}
	return clean
}
