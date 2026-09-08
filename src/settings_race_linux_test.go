//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
