//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type blockingSettings struct {
	started chan struct{}
	release chan struct{}
}

func (b blockingSettings) MarshalJSON() ([]byte, error) {
	close(b.started)
	<-b.release
	return []byte(`{"version":1}`), nil
}

func TestSecureStoreCloseWaitsForActiveWrite(t *testing.T) {
	store, err := newSecureStore(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- store.writeJSON("state.json", blockingSettings{started: started, release: release})
	}()
	<-started
	closeDone := make(chan error, 1)
	go func() { closeDone <- store.close() }()
	select {
	case err := <-closeDone:
		t.Fatalf("close raced active write: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	if err := store.validateDirectory(); err == nil {
		t.Fatal("closed store remained usable")
	}
}

func TestSettingsRenameSyncFailureIsRecoverablyCommitted(t *testing.T) {
	for _, failCall := range []int{1, 2, 3} {
		t.Run(fmt.Sprintf("directory_sync_%d", failCall), func(t *testing.T) {
			store, err := newSecureStore(filepath.Join(t.TempDir(), "auth"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = store.close() }()
			identity := accountIdentity("fault")
			first := settingsFile{Version: 1, Accounts: map[string]accountSetting{identity: {Name: "first", Plan: "pro"}}}
			if err := store.saveSettings(first); err != nil {
				t.Fatal(err)
			}
			calls := 0
			store.dirSync = func(dir *os.File) error {
				calls++
				if calls == failCall {
					return errors.New("injected directory sync failure")
				}
				return dir.Sync()
			}
			second := settingsFile{Version: 1, Accounts: map[string]accountSetting{identity: {Name: "second", Plan: "pro"}}}
			if err := store.saveSettings(second); err != nil {
				t.Fatalf("recoverable post-rename sync failure was reported as rejection: %v", err)
			}
			store.dirSync = nil
			recovered, err := store.recoverSettings()
			if err != nil {
				t.Fatal(err)
			}
			if recovered.Accounts[identity].Name != "second" {
				t.Fatalf("recovery did not preserve acknowledged settings: %#v", recovered)
			}
		})
	}
}

func TestSettingsMarkerRemovalSyncFailureRequiresDurabilityBeforeSuccess(t *testing.T) {
	store, err := newSecureStore(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.close() }()
	identity := accountIdentity("marker-removal-durability")
	previous := settingsFile{Version: 1, Accounts: map[string]accountSetting{identity: {Name: "previous", Plan: "pro"}}}
	if err := store.saveSettings(previous); err != nil {
		t.Fatal(err)
	}
	calls := 0
	store.dirSync = func(dir *os.File) error {
		calls++
		if calls >= 3 {
			return errors.New("injected directory sync failure")
		}
		return dir.Sync()
	}
	desired := settingsFile{Version: 1, Accounts: map[string]accountSetting{identity: {Name: "desired", Plan: "pro"}}}
	if err := store.saveSettings(desired); err == nil || !strings.Contains(err.Error(), "restore settings recovery") {
		t.Fatalf("marker-removal durability failure = %v after %d sync calls, want rejection", err, calls)
	}
}

func TestSettingsRecoveryRollsAcknowledgedUpdateForwardFromPreviousDigest(t *testing.T) {
	store, err := newSecureStore(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.close() }()
	identity := accountIdentity("roll-forward")
	previous := settingsFile{Version: 1, Accounts: map[string]accountSetting{identity: {Name: "previous", Plan: "pro"}}}
	if err := store.saveSettings(previous); err != nil {
		t.Fatal(err)
	}
	desired := settingsFile{Version: 1, Accounts: map[string]accountSetting{identity: {Name: "desired", Plan: "pro"}}}
	recovery := settingsRecovery{Version: settingsRecoveryVersion, PreviousDigest: settingsDigest(previous), Desired: desired}
	if err := store.writeJSON(settingsRecoveryName, recovery); err != nil {
		t.Fatal(err)
	}

	recovered, err := store.recoverSettings()
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Accounts[identity].Name != "desired" {
		t.Fatalf("recovery kept previous settings: %#v", recovered)
	}
	loaded, err := store.loadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Accounts[identity].Name != "desired" {
		t.Fatalf("roll-forward was not persisted: %#v", loaded)
	}
}

func TestSecureStoreWriteConfinedDuringDirectoryReplacement(t *testing.T) {
	root := t.TempDir()
	store, err := newSecureStore(filepath.Join(root, "auth"))
	if err != nil {
		t.Fatal(err)
	}
	original := store.dir + ".original"
	target := filepath.Join(t.TempDir(), "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- store.writeJSON("settings.json", blockingSettings{started: started, release: release})
	}()
	<-started
	if err := os.Rename(store.dir, original); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, store.dir); err != nil {
		t.Fatal(err)
	}
	close(release)

	if err := <-result; err == nil || (!strings.Contains(err.Error(), "replaced") && !strings.Contains(err.Error(), "not a directory")) {
		t.Fatalf("save error = %v, want replaced directory rejection", err)
	}
	if _, err := os.Stat(filepath.Join(target, "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("redirected write reached symlink target: %v", err)
	}
	if _, err := os.Stat(filepath.Join(original, "settings.json")); err != nil {
		t.Fatalf("descriptor-relative write was not confined to original directory: %v", err)
	}
}
