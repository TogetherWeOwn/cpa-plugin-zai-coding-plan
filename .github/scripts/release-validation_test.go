package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateVersion(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		tag     string
		version string
		wantErr bool
	}{
		{name: "release", tag: "v0.1.0", version: "0.1.0"},
		{name: "prerelease", tag: "v0.1.0-rc.1", version: "0.1.0-rc.1"},
		{name: "tag mismatch", tag: "v0.1.1", version: "0.1.0", wantErr: true},
		{name: "missing tag prefix", tag: "0.1.0", version: "0.1.0", wantErr: true},
		{name: "leading zero", tag: "v0.01.0", version: "0.01.0", wantErr: true},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := validateVersion(test.tag, test.version)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateVersion() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestValidateRelease(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeReleaseFixture(t, root, "0.1.0")
	if err := validateRelease(root, "dist", "v0.1.0", "0.1.0"); err != nil {
		t.Fatal(err)
	}
}

func TestValidateReleaseRejectsRegistryMismatch(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeReleaseFixture(t, root, "0.1.0")
	writeRegistry(t, root, "0.1.1")
	if err := validateRelease(root, "dist", "v0.1.0", "0.1.0"); err == nil || !strings.Contains(err.Error(), "registry version") {
		t.Fatalf("validateRelease() error = %v, want registry version error", err)
	}
}

func TestValidateReleaseRequiresChecksumsForBothArtifacts(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeReleaseFixture(t, root, "0.1.0")
	archive := filepath.Join(root, "dist", pluginID+"_0.1.0_linux_amd64.zip")
	writeChecksums(t, filepath.Join(root, "dist", "checksums.txt"), []string{archive})
	if err := validateRelease(root, "dist", "v0.1.0", "0.1.0"); err == nil || !strings.Contains(err.Error(), "want 2") {
		t.Fatalf("validateRelease() error = %v, want missing artifact checksum error", err)
	}
}

func TestValidateReleaseRejectsArchiveMismatch(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeReleaseFixture(t, root, "0.1.0")
	archive := filepath.Join(root, "dist", pluginID+"_0.1.0_linux_amd64.zip")
	writeArchive(t, archive, []byte("different"))
	library := filepath.Join(root, "dist", pluginID+"-v0.1.0.so")
	writeChecksums(t, filepath.Join(root, "dist", "checksums.txt"), []string{library, archive})
	if err := validateRelease(root, "dist", "v0.1.0", "0.1.0"); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("validateRelease() error = %v, want archive mismatch error", err)
	}
}

func TestScanSecrets(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := scanSecrets(root, nil); err == nil || !strings.Contains(err.Error(), "no files") {
		t.Fatalf("scanSecrets() error = %v, want no files error", err)
	}

	if err := os.WriteFile(filepath.Join(root, "safe.txt"), []byte("token: supplied by the runtime\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := scanSecrets(root, []string{"safe.txt"}); err != nil {
		t.Fatal(err)
	}

	secret := "-----BEGIN " + "PRIVATE KEY-----"
	if err := os.WriteFile(filepath.Join(root, "secret.txt"), []byte(secret), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := scanSecrets(root, []string{"safe.txt", "secret.txt"}); err == nil || !strings.Contains(err.Error(), "private key") {
		t.Fatalf("scanSecrets() error = %v, want private key error", err)
	}
}

func TestValidateSourceScansUntrackedFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeDocumentation(t, root)
	command := exec.Command("git", "init", "-q", root)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	if err := os.WriteFile(filepath.Join(root, "source.go"), []byte("package source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateSource(root); err != nil {
		t.Fatal(err)
	}
}

func writeReleaseFixture(t *testing.T, root, version string) {
	t.Helper()
	writeDocumentation(t, root)
	writeRegistry(t, root, version)
	dist := filepath.Join(root, "dist")
	if err := os.MkdirAll(dist, 0o755); err != nil {
		t.Fatal(err)
	}
	library := filepath.Join(dist, pluginID+"-v"+version+".so")
	archive := filepath.Join(dist, pluginID+"_"+version+"_linux_amd64.zip")
	if err := os.WriteFile(library, []byte("shared-library"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeArchive(t, archive, []byte("shared-library"))
	writeChecksums(t, filepath.Join(dist, "checksums.txt"), []string{library, archive})
}

func writeDocumentation(t *testing.T, root string) {
	t.Helper()
	license := "MIT License\n\nPermission is hereby granted, free of charge.\n"
	if err := os.WriteFile(filepath.Join(root, "LICENSE"), []byte(license), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "NOTICE"), []byte("Copyright 2026 TogetherWeOwn\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changelog := "## [0.1.0] - 2026-09-08\n\n[0.1.0]: https://github.com/TogetherWeOwn/cpa-plugin-zai-coding-plan/releases/tag/v0.1.0\n"
	if err := os.WriteFile(filepath.Join(root, "CHANGELOG.md"), []byte(changelog), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeRegistry(t *testing.T, root, version string) {
	t.Helper()
	value := registry{Plugins: []registryPlugin{{
		ID:      pluginID,
		Version: version,
		License: "MIT",
		Release: registryRelease{
			Archive:   pluginID + "_" + version + "_linux_amd64.zip",
			Checksums: "checksums.txt",
		},
	}}}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "registry.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeArchive(t *testing.T, path string, data []byte) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	header := &zip.FileHeader{Name: libraryName, Method: zip.Deflate}
	header.SetMode(0o755)
	entry, err := writer.CreateHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeChecksums(t *testing.T, path string, artifacts []string) {
	t.Helper()
	var lines strings.Builder
	for _, artifact := range artifacts {
		raw, err := os.ReadFile(artifact)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		lines.WriteString(hex.EncodeToString(sum[:]))
		lines.WriteString("  ")
		lines.WriteString(filepath.Base(artifact))
		lines.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(lines.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}
