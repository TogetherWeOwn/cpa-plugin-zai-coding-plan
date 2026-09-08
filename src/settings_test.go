package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func boolPointer(value bool) *bool {
	return &value
}

func TestSecureStorePermissionsAndReplacement(t *testing.T) {
	authDir := filepath.Join(t.TempDir(), "auth")
	store, err := newSecureStore(authDir)
	if err != nil {
		t.Fatal(err)
	}
	if info, errStat := os.Stat(store.dir); errStat != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("store directory mode = %v, err = %v", info.Mode().Perm(), errStat)
	}

	settings := settingsFile{Version: 1, Accounts: map[string]accountSetting{"identity": {Name: "first", Plan: "pro"}}}
	if errSave := store.saveSettings(settings); errSave != nil {
		t.Fatal(errSave)
	}
	path := filepath.Join(store.dir, "settings.json")
	firstInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if firstInfo.Mode().Perm() != 0o600 {
		t.Fatalf("settings mode = %v, want 0600", firstInfo.Mode().Perm())
	}

	settings.Accounts["identity"] = accountSetting{Name: "second", Plan: "pro"}
	if errSave := store.saveSettings(settings); errSave != nil {
		t.Fatal(errSave)
	}
	secondInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(firstInfo, secondInfo) {
		t.Fatal("atomic replacement did not replace the inode")
	}
	loaded, err := store.loadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Accounts["identity"].Name != "second" {
		t.Fatalf("loaded settings = %#v", loaded)
	}
}

func TestSecureStoreCorruption(t *testing.T) {
	store, err := newSecureStore(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.dir, "state.json")
	if errWrite := os.WriteFile(path, []byte("{not-json"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	_, err = store.loadState()
	if err == nil || !strings.Contains(err.Error(), "corrupt state") || strings.Contains(err.Error(), "not-json") {
		t.Fatalf("error = %v, want redacted corruption error", err)
	}
}

func TestSecureStoreRejectsSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permissions vary on Windows")
	}
	store, err := newSecureStore(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if errWrite := os.WriteFile(target, []byte("safe"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	path := filepath.Join(store.dir, "settings.json")
	if errLink := os.Symlink(target, path); errLink != nil {
		t.Fatal(errLink)
	}
	if errSave := store.saveSettings(settingsFile{Version: 1}); errSave == nil || !strings.Contains(errSave.Error(), "symlink") {
		t.Fatalf("save error = %v, want symlink rejection", errSave)
	}
	if _, errLoad := store.loadSettings(); errLoad == nil {
		t.Fatal("load accepted symlink")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "safe" {
		t.Fatalf("symlink target changed to %q", data)
	}
}

func TestFixtureKeyAbsentFromSerializedOutputs(t *testing.T) {
	accounts, err := discoverAccounts(exactPairFixture(fixtureKey), pluginConfig{DefaultPlan: "pro"})
	if err != nil {
		t.Fatal(err)
	}
	outputs := []any{
		accounts,
		settingsFile{Version: 1, Accounts: map[string]accountSetting{accounts[0].Identity: {Name: accounts[0].Name, Plan: accounts[0].Plan}}},
		persistedState{Version: 1, Accounts: map[string]json.RawMessage{accounts[0].Identity: json.RawMessage(`{"health":"healthy"}`)}},
	}
	for _, output := range outputs {
		raw, errMarshal := json.Marshal(output)
		if errMarshal != nil {
			t.Fatal(errMarshal)
		}
		if strings.Contains(string(raw), fixtureKey) {
			t.Fatalf("serialized output leaked fixture key: %s", raw)
		}
	}
}

func TestStoredSettingsValidation(t *testing.T) {
	accounts, err := discoverAccounts(exactPairFixture(fixtureKey), pluginConfig{DefaultPlan: "pro"})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		settings settingsFile
	}{
		{name: "unsupported version", settings: settingsFile{Version: 2}},
		{name: "invalid identity", settings: settingsFile{Version: 1, Accounts: map[string]accountSetting{"not-a-hash": {Plan: "pro"}}}},
		{name: "invalid plan", settings: settingsFile{Version: 1, Accounts: map[string]accountSetting{accounts[0].Identity: {Plan: "enterprise"}}}},
		{name: "negative bucket", settings: settingsFile{Version: 1, Accounts: map[string]accountSetting{accounts[0].Identity: {Plan: "pro", FiveHourCredits: -1}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			copyAccounts := append([]account(nil), accounts...)
			if errApply := applyStoredSettings(copyAccounts, tt.settings); errApply == nil {
				t.Fatal("invalid stored settings succeeded")
			}
		})
	}
}

func TestStoredSettingsPreserveConfigDisabledWhenUnset(t *testing.T) {
	accounts, err := discoverAccounts(exactPairFixture(fixtureKey), pluginConfig{Accounts: []accountOverride{{KeySuffix: "4f9c31a7", Plan: "pro", Disabled: true}}})
	if err != nil {
		t.Fatal(err)
	}
	settings := settingsFile{Version: 1, Accounts: map[string]accountSetting{
		accounts[0].Identity: {Name: "renamed"},
	}}
	if errApply := applyStoredSettings(accounts, settings); errApply != nil {
		t.Fatal(errApply)
	}
	if !accounts[0].Disabled {
		t.Fatal("stored metadata re-enabled a config-disabled account")
	}
}

func TestStoredSettingsCanExplicitlyEnableAccount(t *testing.T) {
	accounts, err := discoverAccounts(exactPairFixture(fixtureKey), pluginConfig{Accounts: []accountOverride{{KeySuffix: "4f9c31a7", Plan: "pro", Disabled: true}}})
	if err != nil {
		t.Fatal(err)
	}
	settings := settingsFile{Version: 1, Accounts: map[string]accountSetting{
		accounts[0].Identity: {Disabled: boolPointer(false)},
	}}
	if errApply := applyStoredSettings(accounts, settings); errApply != nil {
		t.Fatal(errApply)
	}
	if accounts[0].Disabled {
		t.Fatal("explicit stored enabled override was not applied")
	}
}

func TestStoredSettingsRejectDuplicateNames(t *testing.T) {
	fixture := exactPairFixture("key-one-111111")
	fixture.ClaudeKeys = append(fixture.ClaudeKeys, sdkconfig.ClaudeKey{APIKey: "key-two-222222", BaseURL: zaiAnthropicBaseURL})
	fixture.OpenAICompatibility[0].APIKeyEntries = append(fixture.OpenAICompatibility[0].APIKeyEntries, sdkconfig.OpenAICompatibilityAPIKey{APIKey: "key-two-222222"})
	accounts, err := discoverAccounts(fixture, pluginConfig{DefaultPlan: "pro"})
	if err != nil {
		t.Fatal(err)
	}
	settings := settingsFile{Version: 1, Accounts: map[string]accountSetting{
		accounts[0].Identity: {Name: "same"},
		accounts[1].Identity: {Name: "SAME"},
	}}
	if errApply := applyStoredSettings(accounts, settings); errApply == nil || !strings.Contains(errApply.Error(), "duplicate account name") {
		t.Fatalf("error = %v, want duplicate account name", errApply)
	}
}

func TestNewSecureStoreRequiresAbsoluteAuthDir(t *testing.T) {
	_, err := newSecureStore("relative")
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("error = %v, want absolute path error", err)
	}
}
