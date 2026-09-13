package securestore

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type sampleDoc struct {
	Version int    `json:"version"`
	Name    string `json:"name,omitempty"`
}

func TestStorePermissionsAndReplacement(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info, errStat := os.Stat(store.dir); errStat != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("store directory mode = %v, err = %v", info.Mode().Perm(), errStat)
	}

	doc := sampleDoc{Version: 1, Name: "first"}
	if err := store.WriteJSON("doc.json", doc); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.dir, "doc.json")
	firstInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if firstInfo.Mode().Perm() != 0o600 {
		t.Fatalf("doc mode = %v, want 0600", firstInfo.Mode().Perm())
	}

	doc.Name = "second"
	if err := store.WriteJSON("doc.json", doc); err != nil {
		t.Fatal(err)
	}
	secondInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(firstInfo, secondInfo) {
		t.Fatal("atomic replacement did not replace the inode")
	}
	var loaded sampleDoc
	if err := store.ReadJSON("doc.json", &loaded); err != nil {
		t.Fatal(err)
	}
	if loaded.Name != "second" {
		t.Fatalf("loaded doc = %#v", loaded)
	}
}

func TestStoreCorruption(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.dir, "doc.json")
	if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var loaded sampleDoc
	err = store.ReadJSON("doc.json", &loaded)
	if err == nil || !strings.Contains(err.Error(), "corrupt state") || strings.Contains(err.Error(), "not-json") {
		t.Fatalf("error = %v, want redacted corruption error", err)
	}
}

func TestStoreRejectsSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permissions vary on Windows")
	}
	store, err := New(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.dir, "doc.json")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteJSON("doc.json", sampleDoc{Version: 1}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("write error = %v, want symlink rejection", err)
	}
	var loaded sampleDoc
	if err := store.ReadJSON("doc.json", &loaded); err == nil {
		t.Fatal("read accepted symlink")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "safe" {
		t.Fatalf("symlink target changed to %q", data)
	}
}

func TestStoreRejectsInsecureFilePermissions(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.dir, "doc.json")
	if err := os.WriteFile(path, []byte("{\"version\":1}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var loaded sampleDoc
	if err := store.ReadJSON("doc.json", &loaded); err == nil || !strings.Contains(err.Error(), "insecure permissions") {
		t.Fatalf("error = %v, want insecure permissions", err)
	}
}

func TestStoreRejectsDirectoryReplacementWithSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permissions vary on Windows")
	}
	root := t.TempDir()
	store, err := New(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	original := store.dir + ".original"
	if err := os.Rename(store.dir, original); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, store.dir); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteJSON("doc.json", sampleDoc{Version: 1}); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("write error = %v, want replaced directory rejection", err)
	}
	if _, err := os.Stat(filepath.Join(target, "doc.json")); !os.IsNotExist(err) {
		t.Fatalf("redirected write reached symlink target: %v", err)
	}
}

func TestStoreRejectsMultipleJSONDocuments(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.dir, "doc.json")
	if err := os.WriteFile(path, []byte("{\"version\":1} {\"version\":1}"), 0o600); err != nil {
		t.Fatal(err)
	}
	var loaded sampleDoc
	if err := store.ReadJSON("doc.json", &loaded); err == nil || !strings.Contains(err.Error(), "corrupt state") {
		t.Fatalf("error = %v, want corrupt state", err)
	}
}

func TestNewRequiresAbsoluteDir(t *testing.T) {
	_, err := New("relative")
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("error = %v, want absolute path error", err)
	}
}

func TestEnsureEmptyProviderDirRequiresAbsoluteDir(t *testing.T) {
	err := EnsureEmptyProviderDir("relative")
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("error = %v, want absolute path error", err)
	}
}

func TestEnsureEmptyProviderDirCreatesSecureEmptyDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "providers", "opencode-go")
	if err := EnsureEmptyProviderDir(dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("provider dir mode = %v, isDir = %v", info.Mode().Perm(), info.IsDir())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("provider dir is not empty: %v", entries)
	}
}
