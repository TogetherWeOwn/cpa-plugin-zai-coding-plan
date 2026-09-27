//go:build linux

package securestore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type blockingDoc struct {
	started chan struct{}
	release chan struct{}
}

func (b blockingDoc) MarshalJSON() ([]byte, error) {
	close(b.started)
	<-b.release
	return []byte(`{"version":1}`), nil
}

func TestStoreCloseWaitsForActiveWrite(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- store.WriteJSON("doc.json", blockingDoc{started: started, release: release})
	}()
	<-started
	closeDone := make(chan error, 1)
	go func() { closeDone <- store.Close() }()
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
	if err := store.ValidateDirectory(); err == nil {
		t.Fatal("closed store remained usable")
	}
}

func TestStoreReadConfinedDuringDirectoryReplacement(t *testing.T) {
	root := t.TempDir()
	store, err := New(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	trusted := sampleDoc{Version: 1}
	if err := store.WriteJSON("doc.json", trusted); err != nil {
		t.Fatal(err)
	}

	original := store.dir + ".original"
	attacker := filepath.Join(t.TempDir(), "attacker")
	if err := os.Mkdir(attacker, 0o700); err != nil {
		t.Fatal(err)
	}
	malicious := []byte(`{"version":1,"name":"attacker"}` + "\n")
	if err := os.WriteFile(filepath.Join(attacker, "doc.json"), malicious, 0o600); err != nil {
		t.Fatal(err)
	}
	store.fileOpen = func(dir *os.File, name string) (*os.File, error) {
		if err := os.Rename(store.dir, original); err != nil {
			return nil, err
		}
		if err := os.Symlink(attacker, store.dir); err != nil {
			return nil, err
		}
		return openFileAt(dir, name)
	}

	var loaded sampleDoc
	found, err := store.ReadJSONIfExists("doc.json", &loaded)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("trusted doc was not found")
	}
	if loaded.Name != "" {
		t.Fatalf("read followed replacement path and loaded attacker doc: %#v", loaded)
	}
}

func TestStoreWriteConfinedDuringDirectoryReplacement(t *testing.T) {
	root := t.TempDir()
	store, err := New(filepath.Join(root, "store"))
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
		result <- store.WriteJSON("doc.json", blockingDoc{started: started, release: release})
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
		t.Fatalf("write error = %v, want replaced directory rejection", err)
	}
	if _, err := os.Stat(filepath.Join(target, "doc.json")); !os.IsNotExist(err) {
		t.Fatalf("redirected write reached symlink target: %v", err)
	}
	if _, err := os.Stat(filepath.Join(original, "doc.json")); err != nil {
		t.Fatalf("descriptor-relative write was not confined to original directory: %v", err)
	}
}

func TestWriteDirSyncFailureAfterRenameReturnsNeedsRecovery(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	store.dirSync = func(*os.File) error {
		return os.ErrClosed
	}
	err = store.WriteJSON("doc.json", sampleDoc{Version: 1})
	if err == nil || OutcomeOf(err) != WriteNeedsRecovery {
		t.Fatalf("error = %v, want WriteNeedsRecovery outcome", err)
	}
}
