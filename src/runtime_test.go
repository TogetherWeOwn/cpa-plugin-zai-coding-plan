package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeLoadsSettingsByStableIdentity(t *testing.T) {
	root := t.TempDir()
	authDir := filepath.Join(root, "auth")
	configPath := filepath.Join(root, "config.yaml")
	writeCPAConfigFixture(t, configPath, authDir, fixtureKey)

	store, err := newSecureStore(authDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.saveSettings(settingsFile{Version: 1, Accounts: map[string]accountSetting{
		accountIdentity(fixtureKey): {
			Name:            "persisted-name",
			Plan:            "custom",
			Disabled:        boolPointer(true),
			FiveHourCredits: 77,
			WeeklyCredits:   999,
		},
	}}); err != nil {
		t.Fatal(err)
	}

	var runtime pluginRuntime
	if err := runtime.reconfigure([]byte("cpa-config-path: " + configPath + "\ndefault-plan: pro\n")); err != nil {
		t.Fatal(err)
	}
	snapshot, err := runtime.current()
	if err != nil {
		t.Fatal(err)
	}
	account := snapshot.Accounts[0]
	if account.Name != "persisted-name" || account.Plan != "custom" || !account.Disabled || account.FiveHourCredits != 77 || account.WeeklyCredits != 999 {
		t.Fatalf("stored settings not applied: %#v", account)
	}
}

func TestRuntimeRejectsNonExclusiveSchedulerDeployment(t *testing.T) {
	root := t.TempDir()
	cpaPath := filepath.Join(root, "config.yaml")
	writeCPAConfigFixture(t, cpaPath, filepath.Join(root, "auth"), fixtureKey)
	raw, err := os.ReadFile(cpaPath)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.Replace(string(raw), "      priority: 1000\n", "      priority: 1000\n    competitor:\n      enabled: true\n      priority: 2000\n", 1))
	if err = os.WriteFile(cpaPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	var runtime pluginRuntime
	err = runtime.reconfigure([]byte("cpa-config-path: " + cpaPath + "\ndefault-plan: pro\n"))
	if err == nil || !strings.Contains(err.Error(), "sole enabled") {
		t.Fatalf("non-exclusive reconfigure error = %v", err)
	}
	if _, currentErr := runtime.current(); currentErr == nil {
		t.Fatal("non-exclusive deployment published a snapshot")
	}
}

func TestRuntimeReconfigureClosesSupersededAndShutdownClosesFinalStore(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	writeCPAConfigFixture(t, configPath, filepath.Join(root, "auth-one"), fixtureKey)
	rawConfig := []byte("cpa-config-path: " + configPath + "\ndefault-plan: pro\n")
	var runtime pluginRuntime
	if err := runtime.reconfigure(rawConfig); err != nil {
		t.Fatal(err)
	}
	first := runtime.snapshot.Store
	writeCPAConfigFixture(t, configPath, filepath.Join(root, "auth-two"), fixtureKey)
	if err := runtime.reconfigure(rawConfig); err != nil {
		t.Fatal(err)
	}
	if err := first.validateDirectory(); err == nil {
		t.Fatal("superseded secure store remained open")
	}
	final := runtime.snapshot.Store
	if err := runtime.shutdown(); err != nil {
		t.Fatal(err)
	}
	if err := final.validateDirectory(); err == nil {
		t.Fatal("final secure store remained open after shutdown")
	}
}

func TestRuntimeReconfigureKeepsLastValidSnapshot(t *testing.T) {
	root := t.TempDir()
	cpaPath := filepath.Join(root, "config.yaml")
	writeCPAConfigFixture(t, cpaPath, filepath.Join(root, "auth"), fixtureKey)

	var runtime pluginRuntime
	valid := []byte("cpa-config-path: " + cpaPath + "\ndefault-plan: pro\n")
	if err := runtime.reconfigure(valid); err != nil {
		t.Fatal(err)
	}
	before, err := runtime.current()
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Accounts) != 1 {
		t.Fatalf("valid snapshot = %#v", before)
	}

	if err := runtime.reconfigure([]byte("threshold-percent: 0\n")); err == nil {
		t.Fatal("invalid reconfigure succeeded")
	}
	after, err := runtime.current()
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Accounts) != 1 || after.Accounts[0].Identity != before.Accounts[0].Identity {
		t.Fatalf("last valid snapshot changed: before %#v after %#v", before, after)
	}
}

func TestRuntimeValidationStatusIsBounded(t *testing.T) {
	var runtime pluginRuntime
	long := make([]byte, 400)
	for i := range long {
		long[i] = 'x'
	}
	_ = runtime.recordError(fmt.Errorf("validation failed: %s", long))
	status := runtime.validationStatus()
	if len(status) != 240 {
		t.Fatalf("status length = %d, want 240", len(status))
	}
}

func TestRuntimeValidationStatusRedactsProviderKeys(t *testing.T) {
	var runtime pluginRuntime
	_ = runtime.recordError(fmt.Errorf("validation failed for %s", fixtureKey), fixtureKey)
	status := runtime.validationStatus()
	if strings.Contains(status, fixtureKey) {
		t.Fatalf("validation status leaked provider key: %q", status)
	}
	if !strings.Contains(status, "[redacted]") {
		t.Fatalf("validation status = %q, want redaction marker", status)
	}
}

func TestRuntimeShutdownStopsReconfigure(t *testing.T) {
	var runtime pluginRuntime
	if err := runtime.shutdown(); err != nil {
		t.Fatal(err)
	}
	if err := runtime.reconfigure([]byte("threshold-percent: 97\n")); err == nil {
		t.Fatal("reconfigure succeeded after shutdown")
	}
}

func writeCPAConfigFixture(t *testing.T, path, authDir, key string) {
	t.Helper()
	raw := "auth-dir: " + authDir + "\n" +
		"plugins:\n" +
		"  enabled: true\n" +
		"  configs:\n" +
		"    zai-coding-plan:\n" +
		"      enabled: true\n" +
		"      priority: 1000\n" +
		"claude-api-key:\n" +
		"  - api-key: " + key + "\n" +
		"    base-url: " + zaiAnthropicBaseURL + "\n" +
		"openai-compatibility:\n" +
		"  - name: " + zaiCompatName + "\n" +
		"    base-url: " + zaiOpenAIBaseURL + "\n" +
		"    api-key-entries:\n" +
		"      - api-key: " + key + "\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}
