package main

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var operatorModes = map[string]os.FileMode{
	"compatibility-evidence.json": 0o644,
	"config.yaml.tmpl":            0o644,
	"registry.json":               0o644,
	"router-capacity-source.json": 0o644,
	"verify-live.sh":              0o755,
	"prepare-usage-dir.py":        0o755,
	"remove-usage-output.py":      0o755,
	"rollback.sh":                 0o755,
	"README.md":                   0o644,
	"release-sha.txt":             0o644,
}

func TestValidateOperatorBundleAcceptsStrictManifest(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dist := filepath.Join(root, "dist")
	writeOperatorFixture(t, root, dist, nil)
	if err := validateOperatorBundle(root, dist, "0.2.0"); err != nil {
		t.Fatal(err)
	}
}

func TestValidateOperatorBundleRejectsHostileArchives(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		mutate  func(entries *[]operatorTestEntry)
		message string
	}{
		{name: "traversal", mutate: func(entries *[]operatorTestEntry) { (*entries)[0].name = "../compatibility-evidence.json" }, message: "unsafe entry"},
		{name: "nested path", mutate: func(entries *[]operatorTestEntry) { (*entries)[0].name = "nested/compatibility-evidence.json" }, message: "unsafe entry"},
		{name: "unexpected", mutate: func(entries *[]operatorTestEntry) {
			*entries = append(*entries, operatorTestEntry{name: "extra.txt", body: "extra", mode: 0o644})
		}, message: "unexpected entry"},
		{name: "duplicate", mutate: func(entries *[]operatorTestEntry) { *entries = append(*entries, (*entries)[0]) }, message: "duplicate entry"},
		{name: "missing", mutate: func(entries *[]operatorTestEntry) { *entries = (*entries)[1:] }, message: "want 10"},
		{name: "wrong mode", mutate: func(entries *[]operatorTestEntry) { (*entries)[0].mode = 0o600 }, message: "has mode"},
		{name: "changed bytes", mutate: func(entries *[]operatorTestEntry) {
			for index := range *entries {
				if (*entries)[index].name == "compatibility-evidence.json" {
					(*entries)[index].body = "changed"
					break
				}
			}
		}, message: "does not match staged artifact"},
		{name: "unresolved placeholder", mutate: func(entries *[]operatorTestEntry) {
			for index := range *entries {
				if (*entries)[index].name == "README.md" {
					(*entries)[index].body = "${RELEASE_SHA}"
					break
				}
			}
		}, message: "unresolved release placeholders"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			dist := filepath.Join(root, "dist")
			writeOperatorFixture(t, root, dist, test.mutate)
			err := validateOperatorBundle(root, dist, "0.2.0")
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("validateOperatorBundle() error = %v, want %q", err, test.message)
			}
		})
	}
}

func TestValidateReleaseSHA(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "commit", value: strings.Repeat("a", 40) + "\n"},
		{name: "tag object format still accepted", value: strings.Repeat("b", 40) + "\n"},
		{name: "short", value: "abc\n", wantErr: true},
		{name: "uppercase", value: strings.Repeat("A", 40) + "\n", wantErr: true},
		{name: "missing newline", value: strings.Repeat("a", 40), wantErr: true},
		{name: "multiple lines", value: strings.Repeat("a", 40) + "\nextra\n", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "release-sha.txt")
			if err := os.WriteFile(path, []byte(test.value), 0o644); err != nil {
				t.Fatal(err)
			}
			err := validateReleaseSHA(path)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateReleaseSHA() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestReleaseWorkflowWritesPeeledAnnotatedTagCommit(t *testing.T) {
	t.Parallel()
	workflow := validReleaseWorkflow(t)
	if !strings.Contains(workflow, `release_sha=$(git rev-parse "$RAW_TAG^{commit}")`) {
		t.Fatal("release workflow does not peel the release tag")
	}
	if !strings.Contains(workflow, `RELEASE_SHA: ${{ steps.release.outputs.release_sha }}`) {
		t.Fatal("release artifact staging does not use the peeled release commit")
	}
	if strings.Contains(workflow, `RELEASE_SHA: ${{ steps.target.outputs.tag_object }}`) {
		t.Fatal("release artifact staging still uses the annotated tag object")
	}
}

type operatorTestEntry struct {
	name string
	body string
	mode os.FileMode
}

func writeOperatorFixture(t *testing.T, root, dist string, mutate func(*[]operatorTestEntry)) {
	t.Helper()
	if err := os.MkdirAll(dist, 0o755); err != nil {
		t.Fatal(err)
	}
	registry := `{"plugins":[]}`
	if err := os.WriteFile(filepath.Join(root, "registry.json"), []byte(registry), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := make([]operatorTestEntry, 0, len(operatorModes))
	for name, mode := range operatorModes {
		body := "fixture-" + name
		if name == "registry.json" {
			body = registry
		}
		if name == "release-sha.txt" {
			body = strings.Repeat("a", 40) + "\n"
		}
		if err := os.WriteFile(filepath.Join(dist, name), []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, operatorTestEntry{name: name, body: body, mode: mode})
	}
	if mutate != nil {
		mutate(&entries)
	}
	for _, item := range entries {
		if item.name == "README.md" || item.name == "config.yaml.tmpl" {
			if err := os.WriteFile(filepath.Join(dist, item.name), []byte(item.body), item.mode); err != nil {
				t.Fatal(err)
			}
		}
	}
	path := filepath.Join(dist, pluginID+"-v0.2.0-operator.zip")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	for _, item := range entries {
		header := &zip.FileHeader{Name: item.name, Method: zip.Deflate}
		header.SetMode(item.mode)
		entry, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(item.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
