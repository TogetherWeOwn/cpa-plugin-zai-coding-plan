package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
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
		{name: "make expression", tag: "v$(shell,id)", version: "$(shell,id)", wantErr: true},
		{name: "newline", tag: "v0.1.0\nnext", version: "0.1.0\nnext", wantErr: true},
		{name: "slash", tag: "v0.1.0/path", version: "0.1.0/path", wantErr: true},
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

func TestValidateReleaseRejectsRegistryArchiveMismatch(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeReleaseFixture(t, root, "0.1.0")
	writeRegistryValue(t, root, registryPlugin{
		ID:      pluginID,
		Version: "0.1.0",
		License: "MIT",
		Release: registryRelease{Archive: pluginID + "_0.1.1_linux_amd64.zip", Checksums: "checksums.txt"},
	})
	if err := validateRelease(root, "dist", "v0.1.0", "0.1.0"); err == nil || !strings.Contains(err.Error(), "registry archive") {
		t.Fatalf("validateRelease() error = %v, want registry archive error", err)
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

func TestValidateDocumentationRejectsTruncatedLicense(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeDocumentation(t, root)
	if err := os.WriteFile(filepath.Join(root, "LICENSE"), []byte("MIT License\nPermission is hereby granted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateDocumentation(root); err == nil || !strings.Contains(err.Error(), "canonical MIT license") {
		t.Fatalf("validateDocumentation() error = %v, want canonical license error", err)
	}
}

func TestValidateDocumentationRejectsTruncatedNotice(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeDocumentation(t, root)
	if err := os.WriteFile(filepath.Join(root, "NOTICE"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateDocumentation(root); err == nil || !strings.Contains(err.Error(), "canonical project notice") {
		t.Fatalf("validateDocumentation() error = %v, want canonical NOTICE error", err)
	}
}

func TestValidateDocumentationRejectsReorderedLicense(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeDocumentation(t, root)
	paragraphs := strings.Split(canonicalMITLicense, "\n\n")
	paragraphs[1], paragraphs[2] = paragraphs[2], paragraphs[1]
	if err := os.WriteFile(filepath.Join(root, "LICENSE"), []byte(strings.Join(paragraphs, "\n\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateDocumentation(root); err == nil || !strings.Contains(err.Error(), "canonical MIT license") {
		t.Fatalf("validateDocumentation() error = %v, want reordered license rejection", err)
	}
}

func TestValidateHostImagePinRejectsDigestDrift(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeDocumentation(t, root)
	path := filepath.Join(root, ".github", "release-host-image.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.Replace(string(raw), hostImageAMD64Digest, "sha256:"+strings.Repeat("0", 64), 1))
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateHostImagePin(root); err == nil || !strings.Contains(err.Error(), "approved v7.2.67") {
		t.Fatalf("validateHostImagePin() error = %v, want digest mismatch", err)
	}
}

func TestValidateWorkflowActionPinsRejectsUnpinnedAction(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeWorkflow(t, root, "ci.yml", "steps:\n  - uses: actions/checkout@v7\n")
	if err := validateWorkflowActionPins(root); err == nil || !strings.Contains(err.Error(), "not pinned") {
		t.Fatalf("validateWorkflowActionPins() error = %v, want unpinned action rejection", err)
	}
}

func TestValidateWorkflowActionPinsRejectsFlowSyntax(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeWorkflow(t, root, "ci.yml", "steps:\n  - { uses: attacker/example@main }\n")
	if err := validateWorkflowActionPins(root); err == nil || !strings.Contains(err.Error(), "not pinned") {
		t.Fatalf("validateWorkflowActionPins() error = %v, want flow-style unpinned action rejection", err)
	}
}

func TestValidateWorkflowActionPinsRejectsUnpinnedYAMLWorkflow(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeWorkflow(t, root, "ci.yml", "steps: []\n")
	writeWorkflow(t, root, "bypass.yaml", "steps:\n  - uses: attacker/example@main\n")
	if err := validateWorkflowActionPins(root); err == nil || !strings.Contains(err.Error(), "bypass.yaml") {
		t.Fatalf("validateWorkflowActionPins() error = %v, want .yaml action rejection", err)
	}
}

func TestValidateReleaseWorkflowBoundaryRejectsExtraPublishCommand(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workflow := validReleaseWorkflow + "      - name: Exfiltrate\n        env:\n          GH_TOKEN: ${{ github.token }}\n        run: printf '%s' \"$GH_TOKEN\" >/dev/null\n"
	writeWorkflow(t, root, "release.yml", workflow)
	if err := validateReleaseWorkflowBoundary(root); err == nil || !strings.Contains(err.Error(), "canonical") {
		t.Fatalf("validateReleaseWorkflowBoundary() error = %v, want canonical publication rejection", err)
	}
}

func TestValidateReleaseWorkflowBoundaryRejectsExtraPublishAction(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workflow := strings.Replace(validReleaseWorkflow, "      - name: Publish GitHub release\n", "      - { uses: attacker/example@"+strings.Repeat("a", 40)+" }\n      - name: Publish GitHub release\n", 1)
	writeWorkflow(t, root, "release.yml", workflow)
	if err := validateReleaseWorkflowBoundary(root); err == nil || !strings.Contains(err.Error(), "exactly") {
		t.Fatalf("validateReleaseWorkflowBoundary() error = %v, want extra action rejection", err)
	}
}

func TestValidateReleaseWorkflowBoundaryRejectsPublishDefaults(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workflow := strings.Replace(validReleaseWorkflow, "    runs-on: ubuntu-24.04\n    permissions:\n      contents: write", "    runs-on: ubuntu-24.04\n    defaults:\n      run:\n        shell: bash -c 'printf malicious-side-effect; bash {0}'\n    permissions:\n      contents: write", 1)
	writeWorkflow(t, root, "release.yml", workflow)
	if err := validateReleaseWorkflowBoundary(root); err == nil || !strings.Contains(err.Error(), "unapproved key") {
		t.Fatalf("validateReleaseWorkflowBoundary() error = %v, want publish defaults rejection", err)
	}
}

func TestValidateReleaseWorkflowBoundaryRejectsPublishContainer(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workflow := strings.Replace(validReleaseWorkflow, "    runs-on: ubuntu-24.04\n    permissions:\n      contents: write", "    runs-on: ubuntu-24.04\n    container: attacker.invalid/credential-stealer:latest\n    permissions:\n      contents: write", 1)
	writeWorkflow(t, root, "release.yml", workflow)
	if err := validateReleaseWorkflowBoundary(root); err == nil || !strings.Contains(err.Error(), "unapproved key") {
		t.Fatalf("validateReleaseWorkflowBoundary() error = %v, want publish container rejection", err)
	}
}

func TestValidateReleaseWorkflowBoundaryRejectsPublicationShell(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workflow := strings.Replace(validReleaseWorkflow, "      - name: Publish GitHub release\n        env:", "      - name: Publish GitHub release\n        shell: bash -c 'printf malicious-side-effect; bash {0}'\n        env:", 1)
	writeWorkflow(t, root, "release.yml", workflow)
	if err := validateReleaseWorkflowBoundary(root); err == nil || !strings.Contains(err.Error(), "unapproved key") {
		t.Fatalf("validateReleaseWorkflowBoundary() error = %v, want publication shell rejection", err)
	}
}

func TestValidateReleaseWorkflowBoundaryAcceptsCanonicalWorkflow(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeWorkflow(t, root, "release.yml", validReleaseWorkflow)
	if err := validateReleaseWorkflowBoundary(root); err != nil {
		t.Fatal(err)
	}
}

func writeWorkflow(t *testing.T, root, name, contents string) {
	t.Helper()
	workflowDir := filepath.Join(root, ".github", "workflows")
	if err := os.MkdirAll(workflowDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workflowDir, name), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

const validReleaseWorkflow = `name: Release
permissions:
  contents: read
jobs:
  build:
    runs-on: ubuntu-24.04
    steps: []
  publish:
    needs: build
    runs-on: ubuntu-24.04
    permissions:
      contents: write
    steps:
      - name: Download release artifacts
        uses: actions/download-artifact@d3f86a106a0bac45b974a628896c90dbdf5c8093
        with:
          name: release-artifacts
          path: release-artifacts
      - name: Publish GitHub release
        env:
          GH_TOKEN: ${{ github.token }}
          GH_REPO: ${{ github.repository }}
          VERSION: ${{ needs.build.outputs.version }}
          RAW_TAG: ${{ github.ref_name }}
        run: |
          gh release create "$RAW_TAG" \
            "release-artifacts/zai-coding-plan-v${VERSION}.so" \
            "release-artifacts/zai-coding-plan_${VERSION}_linux_amd64.zip" \
            release-artifacts/checksums.txt \
            --verify-tag --generate-notes
`

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
	if err := os.MkdirAll(filepath.Join(root, ".github"), 0o755); err != nil {
		t.Fatal(err)
	}
	pin := map[string]string{
		"repository":      hostImageRepository,
		"tag":             hostImageTag,
		"platform":        "linux/amd64",
		"manifest_digest": hostImageAMD64Digest,
	}
	pinRaw, err := json.Marshal(pin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".github", "release-host-image.json"), pinRaw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "LICENSE"), []byte(canonicalMITLicense), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "NOTICE"), []byte(canonicalNotice), 0o644); err != nil {
		t.Fatal(err)
	}
	changelog := "## [0.1.0] - 2026-09-08\n\n[0.1.0]: https://github.com/TogetherWeOwn/cpa-plugin-zai-coding-plan/releases/tag/v0.1.0\n"
	if err := os.WriteFile(filepath.Join(root, "CHANGELOG.md"), []byte(changelog), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeRegistry(t *testing.T, root, version string) {
	t.Helper()
	writeRegistryValue(t, root, registryPlugin{
		ID:      pluginID,
		Version: version,
		License: "MIT",
		Release: registryRelease{
			Archive:   pluginID + "_" + version + "_linux_amd64.zip",
			Checksums: "checksums.txt",
		},
	})
}

func writeRegistryValue(t *testing.T, root string, plugin registryPlugin) {
	t.Helper()
	value := registry{Plugins: []registryPlugin{plugin}}
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
