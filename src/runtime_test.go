package main

import (
	"fmt"
	"os"
	"path/filepath"
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
