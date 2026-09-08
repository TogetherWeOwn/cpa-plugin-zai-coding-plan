package main

import (
	"fmt"
	"sync"
)

type runtimeSnapshot struct {
	Config   pluginConfig
	Accounts []account
	Store    *secureStore
}

type pluginRuntime struct {
	mu       sync.RWMutex
	snapshot *runtimeSnapshot
	lastErr  error
}

func (r *pluginRuntime) reconfigure(rawConfig []byte) error {
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
	r.lastErr = err
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
