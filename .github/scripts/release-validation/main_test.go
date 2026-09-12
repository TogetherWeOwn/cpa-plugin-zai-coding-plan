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
		{name: "release", tag: "v0.2.0", version: "0.2.0"},
		{name: "prerelease", tag: "v0.2.0-rc.1", version: "0.2.0-rc.1"},
		{name: "tag mismatch", tag: "v0.1.1", version: "0.2.0", wantErr: true},
		{name: "missing tag prefix", tag: "0.2.0", version: "0.2.0", wantErr: true},
		{name: "leading zero", tag: "v0.01.0", version: "0.01.0", wantErr: true},
		{name: "make expression", tag: "v$(shell,id)", version: "$(shell,id)", wantErr: true},
		{name: "newline", tag: "v0.2.0\nnext", version: "0.2.0\nnext", wantErr: true},
		{name: "slash", tag: "v0.2.0/path", version: "0.2.0/path", wantErr: true},
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
	writeReleaseFixture(t, root, "0.2.0")
	if err := validateRelease(root, "dist", "v0.2.0", "0.2.0"); err != nil {
		t.Fatal(err)
	}
}

func TestValidateReleaseRejectsRegistryMismatch(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeReleaseFixture(t, root, "0.2.0")
	writeRegistry(t, root, "0.1.1")
	if err := validateRelease(root, "dist", "v0.2.0", "0.2.0"); err == nil || !strings.Contains(err.Error(), "registry version") {
		t.Fatalf("validateRelease() error = %v, want registry version error", err)
	}
}

func TestValidateReleaseRejectsRegistryArchiveMismatch(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeReleaseFixture(t, root, "0.2.0")
	writeRegistryValue(t, root, registryPlugin{
		ID:      pluginID,
		Version: "0.2.0",
		License: "MIT",
		Release: registryRelease{Archive: pluginID + "_0.1.1_linux_amd64.zip", Checksums: "checksums.txt"},
	})
	if err := validateRelease(root, "dist", "v0.2.0", "0.2.0"); err == nil || !strings.Contains(err.Error(), "registry archive") {
		t.Fatalf("validateRelease() error = %v, want registry archive error", err)
	}
}

func TestValidateReleaseRequiresChecksumsForBothArtifacts(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeReleaseFixture(t, root, "0.2.0")
	archive := filepath.Join(root, "dist", pluginID+"_0.2.0_linux_amd64.zip")
	writeChecksums(t, filepath.Join(root, "dist", "checksums.txt"), []string{archive})
	if err := validateRelease(root, "dist", "v0.2.0", "0.2.0"); err == nil || !strings.Contains(err.Error(), "want 2") {
		t.Fatalf("validateRelease() error = %v, want missing artifact checksum error", err)
	}
}

func TestValidateReleaseRejectsArchiveMismatch(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeReleaseFixture(t, root, "0.2.0")
	archive := filepath.Join(root, "dist", pluginID+"_0.2.0_linux_amd64.zip")
	writeArchive(t, archive, []byte("different"))
	library := filepath.Join(root, "dist", pluginID+"-v0.2.0.so")
	writeChecksums(t, filepath.Join(root, "dist", "checksums.txt"), []string{library, archive})
	if err := validateRelease(root, "dist", "v0.2.0", "0.2.0"); err == nil || !strings.Contains(err.Error(), "does not match") {
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
	path := filepath.Join(root, ".github", "host-images.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.Replace(string(raw), baselineHostImageAMD64Digest, "sha256:"+strings.Repeat("0", 64), 1))
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

func TestValidateWorkflowActionPinsRejectsAlias(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeWorkflow(t, root, "ci.yml", "unpinned: &unpinned attacker/example@main\nsteps:\n  - uses: *unpinned\n")
	if err := validateWorkflowActionPins(root); err == nil || !strings.Contains(err.Error(), "cannot use YAML aliases") {
		t.Fatalf("validateWorkflowActionPins() error = %v, want aliased action rejection", err)
	}
}

func TestValidateWorkflowActionPinsRejectsAliasedKey(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeWorkflow(t, root, "ci.yml", "name: &uses uses\nsteps:\n  - *uses: attacker/example@main\n")
	if err := validateWorkflowActionPins(root); err == nil || !strings.Contains(err.Error(), "cannot use YAML aliases") {
		t.Fatalf("validateWorkflowActionPins() error = %v, want aliased key rejection", err)
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
	writeReleaseWorkflows(t, root, workflow)
	if err := validateReleaseWorkflowBoundary(root); err == nil || !strings.Contains(err.Error(), "canonical") {
		t.Fatalf("validateReleaseWorkflowBoundary() error = %v, want canonical publication rejection", err)
	}
}

func TestValidateReleaseWorkflowBoundaryRejectsExtraPublishAction(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workflow := strings.Replace(validReleaseWorkflow, "      - name: Publish GitHub release\n", "      - { uses: attacker/example@"+strings.Repeat("a", 40)+" }\n      - name: Publish GitHub release\n", 1)
	writeReleaseWorkflows(t, root, workflow)
	if err := validateReleaseWorkflowBoundary(root); err == nil || !strings.Contains(err.Error(), "exactly") {
		t.Fatalf("validateReleaseWorkflowBoundary() error = %v, want extra action rejection", err)
	}
}

func TestValidateReleaseWorkflowBoundaryRejectsWorkflowDefaults(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workflow := strings.Replace(validReleaseWorkflow, "permissions:\n  contents: read", "defaults:\n  run:\n    shell: bash -c 'printf malicious-side-effect; bash {0}'\npermissions:\n  contents: read", 1)
	writeReleaseWorkflows(t, root, workflow)
	if err := validateReleaseWorkflowBoundary(root); err == nil || !strings.Contains(err.Error(), "unapproved key") {
		t.Fatalf("validateReleaseWorkflowBoundary() error = %v, want workflow defaults rejection", err)
	}
}

func TestValidateReleaseWorkflowBoundaryRejectsPublishDefaults(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workflow := strings.Replace(validReleaseWorkflow, "    runs-on: ubuntu-24.04\n    permissions:\n      contents: write", "    runs-on: ubuntu-24.04\n    defaults:\n      run:\n        shell: bash -c 'printf malicious-side-effect; bash {0}'\n    permissions:\n      contents: write", 1)
	writeReleaseWorkflows(t, root, workflow)
	if err := validateReleaseWorkflowBoundary(root); err == nil || !strings.Contains(err.Error(), "unapproved key") {
		t.Fatalf("validateReleaseWorkflowBoundary() error = %v, want publish defaults rejection", err)
	}
}

func TestValidateReleaseWorkflowBoundaryRejectsBuildShell(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workflow := strings.Replace(validReleaseWorkflow, "    outputs:\n      version:", "    defaults:\n      run:\n        shell: bash -c 'printf malicious-side-effect; bash {0}'\n    outputs:\n      version:", 1)
	writeReleaseWorkflows(t, root, workflow)
	if err := validateReleaseWorkflowBoundary(root); err == nil || !strings.Contains(err.Error(), "unapproved key") {
		t.Fatalf("validateReleaseWorkflowBoundary() error = %v, want build defaults rejection", err)
	}
}

func TestValidateReleaseWorkflowBoundaryRejectsBuildStepShell(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workflow := strings.Replace(validReleaseWorkflow, "      - name: Package\n        working-directory:", "      - name: Package\n        shell: bash -c 'printf malicious-side-effect; bash {0}'\n        working-directory:", 1)
	writeReleaseWorkflows(t, root, workflow)
	if err := validateReleaseWorkflowBoundary(root); err == nil || !strings.Contains(err.Error(), "unapproved key") {
		t.Fatalf("validateReleaseWorkflowBoundary() error = %v, want build step shell rejection", err)
	}
}

func TestValidateReleaseWorkflowBoundaryRejectsPublishContainer(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workflow := strings.Replace(validReleaseWorkflow, "    runs-on: ubuntu-24.04\n    permissions:\n      contents: write", "    runs-on: ubuntu-24.04\n    container: attacker.invalid/credential-stealer:latest\n    permissions:\n      contents: write", 1)
	writeReleaseWorkflows(t, root, workflow)
	if err := validateReleaseWorkflowBoundary(root); err == nil || !strings.Contains(err.Error(), "unapproved key") {
		t.Fatalf("validateReleaseWorkflowBoundary() error = %v, want publish container rejection", err)
	}
}

func TestValidateReleaseWorkflowBoundaryRejectsPublicationShell(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workflow := strings.Replace(validReleaseWorkflow, "      - name: Publish GitHub release\n        env:", "      - name: Publish GitHub release\n        shell: bash -c 'printf malicious-side-effect; bash {0}'\n        env:", 1)
	writeReleaseWorkflows(t, root, workflow)
	if err := validateReleaseWorkflowBoundary(root); err == nil || !strings.Contains(err.Error(), "unapproved key") {
		t.Fatalf("validateReleaseWorkflowBoundary() error = %v, want publication shell rejection", err)
	}
}

func TestValidateReleaseWorkflowBoundaryAcceptsCanonicalWorkflow(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeReleaseWorkflows(t, root, validReleaseWorkflow)
	if err := validateReleaseWorkflowBoundary(root); err != nil {
		t.Fatal(err)
	}
}

func TestValidateReleaseWorkflowBoundaryRejectsBuildCommandMutation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workflow := strings.Replace(validReleaseWorkflow, "          make package VERSION=\"$VERSION\" OUT=\"$library\" ARCHIVE=\"$archive\"", "          make package VERSION=\"$VERSION\" OUT=\"$library\" ARCHIVE=\"$archive\"\n          printf injected >> \"$library\"", 1)
	writeReleaseWorkflows(t, root, workflow)
	if err := validateReleaseWorkflowBoundary(root); err == nil || !strings.Contains(err.Error(), "canonical command") {
		t.Fatalf("validateReleaseWorkflowBoundary() error = %v, want command mutation rejection", err)
	}
}

func TestValidateReleaseWorkflowBoundaryRejectsBuildEnvironmentMutation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workflow := strings.Replace(validReleaseWorkflow, "        env:\n          VERSION: ${{ steps.release.outputs.version }}\n        run: |\n          set -euo pipefail\n          library=", "        env:\n          VERSION: ${{ steps.release.outputs.version }}\n          BASH_ENV: ../release-controls/attacker.sh\n        run: |\n          set -euo pipefail\n          library=", 1)
	writeReleaseWorkflows(t, root, workflow)
	if err := validateReleaseWorkflowBoundary(root); err == nil || !strings.Contains(err.Error(), "canonical environment") {
		t.Fatalf("validateReleaseWorkflowBoundary() error = %v, want environment mutation rejection", err)
	}
}

func TestValidateReleaseWorkflowBoundaryRejectsFoldedBuildCommand(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workflow := strings.Replace(validReleaseWorkflow, "        run: |\n          set -euo pipefail\n          sudo --preserve-env=VERSION,HOST_MATRIX_NAMESPACE make test-host-matrix", "        run: >-\n          set -euo pipefail\n          sudo --preserve-env=VERSION,HOST_MATRIX_NAMESPACE make test-host-matrix", 1)
	writeReleaseWorkflows(t, root, workflow)
	if err := validateReleaseWorkflowBoundary(root); err == nil || !strings.Contains(err.Error(), "literal block style") {
		t.Fatalf("validateReleaseWorkflowBoundary() error = %v, want folded build command rejection", err)
	}
}

func TestValidateReleaseWorkflowBoundaryRejectsChangedRunChomping(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workflow := strings.Replace(validReleaseWorkflow, "        run: |\n          set -euo pipefail\n          sudo --preserve-env=VERSION,HOST_MATRIX_NAMESPACE make test-host-matrix", "        run: |-\n          set -euo pipefail\n          sudo --preserve-env=VERSION,HOST_MATRIX_NAMESPACE make test-host-matrix", 1)
	writeReleaseWorkflows(t, root, workflow)
	if err := validateReleaseWorkflowBoundary(root); err == nil || !strings.Contains(err.Error(), "exactly run: |") {
		t.Fatalf("validateReleaseWorkflowBoundary() error = %v, want changed chomping rejection", err)
	}
}

func TestValidateReleaseWorkflowBoundaryRejectsFoldedPublicationCommand(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workflow := strings.Replace(validReleaseWorkflow, "        run: |\n          set -euo pipefail\n          actual_tag_object=", "        run: >-\n          set -euo pipefail\n          actual_tag_object=", 1)
	writeReleaseWorkflows(t, root, workflow)
	if err := validateReleaseWorkflowBoundary(root); err == nil || !strings.Contains(err.Error(), "literal block style") {
		t.Fatalf("validateReleaseWorkflowBoundary() error = %v, want folded publication command rejection", err)
	}
}

func writeReleaseWorkflows(t *testing.T, root, release string) {
	t.Helper()
	writeWorkflow(t, root, "ci.yml", "name: CI\non:\n  push:\n    branches: [main]\n  pull_request:\n")
	writeWorkflow(t, root, "release.yml", release)
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
on:
  push:
    tags: ["v*"]
permissions:
  contents: read
jobs:
  build:
    runs-on: ubuntu-24.04
    outputs:
      version: ${{ steps.release.outputs.version }}
      tag: ${{ steps.target.outputs.tag }}
      tag_object: ${{ steps.target.outputs.tag_object }}
    steps:
      - name: Check out trusted release controls
        uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1
        with:
          fetch-depth: 0
          persist-credentials: false
          ref: ${{ github.sha }}
          path: release-controls
      - name: Set up Go
        uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e
        with:
          go-version-file: release-controls/go.mod
          cache-dependency-path: release-controls/go.sum
          cache: true
      - name: Validate trusted release controls
        working-directory: release-controls
        run: |
          set -euo pipefail
          go test ./.github/scripts/release-validation
          go run -buildvcs=false ./.github/scripts/release-validation -mode source
          .github/scripts/select-release-tag_test.sh
      - name: Select release tag
        id: target
        working-directory: release-controls
        env:
          EVENT_NAME: ${{ github.event_name }}
          EVENT_REF: ${{ github.ref }}
          EVENT_SHA: ${{ github.sha }}
        run: |
          set -euo pipefail
          raw_tag=$(.github/scripts/select-release-tag.sh \
            "$EVENT_NAME" "$EVENT_REF" "$EVENT_SHA" .)
          tag_object=$(git rev-parse "$raw_tag")
          printf 'tag=%s\n' "$raw_tag" >> "$GITHUB_OUTPUT"
          printf 'tag_object=%s\n' "$tag_object" >> "$GITHUB_OUTPUT"
      - name: Check out immutable release source
        uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1
        with:
          fetch-depth: 0
          persist-credentials: false
          ref: ${{ steps.target.outputs.tag }}
          path: release-source
      - name: Validate tag and provenance
        id: release
        working-directory: release-source
        env:
          RAW_TAG: ${{ steps.target.outputs.tag }}
          EXPECTED_TAG_OBJECT: ${{ steps.target.outputs.tag_object }}
        run: |
          set -euo pipefail
          version=$(go run -buildvcs=false ./.github/scripts/release-validation -mode version -tag "$RAW_TAG")
          test "$(git rev-parse "$RAW_TAG")" = "$EXPECTED_TAG_OBJECT"
          release_sha=$(git rev-parse "$RAW_TAG^{commit}")
          test "$(git rev-parse HEAD)" = "$release_sha"
          git fetch --no-tags origin main
          git merge-base --is-ancestor "$release_sha" origin/main
          printf 'version=%s\n' "$version" >> "$GITHUB_OUTPUT"
      - name: Verify
        working-directory: release-source
        run: |
          make fmt-check
          make vet
          make test
          make test-release
          make validate-source
          make scan-secrets
      - name: Lint
        uses: golangci/golangci-lint-action@ba0d7d2ec06a0ea1cb5fa41b2e4a3ab91d21278a
        with:
          version: v2.13.2
          working-directory: release-source
      - name: Package
        working-directory: release-source
        env:
          VERSION: ${{ steps.release.outputs.version }}
        run: |
          set -euo pipefail
          library="dist/zai-coding-plan-v${VERSION}.so"
          archive="dist/zai-coding-plan_${VERSION}_linux_amd64.zip"
          make package VERSION="$VERSION" OUT="$library" ARCHIVE="$archive"
      - name: Verify plugin-store artifact
        working-directory: release-source
        env:
          VERSION: ${{ steps.release.outputs.version }}
          RAW_TAG: ${{ steps.target.outputs.tag }}
        run: |
          set -euo pipefail
          library="dist/zai-coding-plan-v${VERSION}.so"
          archive="dist/zai-coding-plan_${VERSION}_linux_amd64.zip"
          nm -D "$library" | grep -Eq '[[:space:]]cliproxy_plugin_init$'
          test "$(unzip -Z1 "$archive")" = "zai-coding-plan.so"
          cmp "$library" <(unzip -p "$archive" zai-coding-plan.so)
          go run -buildvcs=false ./.github/scripts/release-validation \
            -mode release -version "$VERSION" -tag "$RAW_TAG"
      - name: Resolve immutable host image matrix
        working-directory: release-source
        run: |
          set -euo pipefail
          .github/scripts/resolve-host-images.sh > "$RUNNER_TEMP/host-images.json"
      - name: Test host compatibility matrix
        working-directory: release-source
        env:
          VERSION: ${{ steps.release.outputs.version }}
          HOST_MATRIX_NAMESPACE: root
        run: |
          set -euo pipefail
          sudo --preserve-env=VERSION,HOST_MATRIX_NAMESPACE make test-host-matrix VERSION="$VERSION" OUT="dist/zai-coding-plan-v${VERSION}.so" HOST_IMAGES="$RUNNER_TEMP/host-images.json" HOST_MATRIX_WORK="$RUNNER_TEMP/host-matrix"
      - name: Stage release artifacts
        working-directory: release-source
        env:
          VERSION: ${{ steps.release.outputs.version }}
          RELEASE_SHA: ${{ steps.target.outputs.tag_object }}
        run: |
          set -euo pipefail
          mkdir release-artifacts
          cp "dist/zai-coding-plan-v${VERSION}.so" \
             "dist/zai-coding-plan_${VERSION}_linux_amd64.zip" \
             dist/checksums.txt release-artifacts/
          cp "$RUNNER_TEMP/host-images.json" release-artifacts/compatibility-evidence.json
          cp deploy/config.yaml.tmpl deploy/router-capacity-source.json deploy/verify-live.sh \
             deploy/prepare-usage-dir.py deploy/remove-usage-output.py deploy/rollback.sh deploy/README.md release-artifacts/
          printf '%s\n' "$RELEASE_SHA" > release-artifacts/release-sha.txt
      - name: Upload release artifacts
        uses: actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02
        with:
          name: release-artifacts
          path: release-source/release-artifacts/
          if-no-files-found: error
          retention-days: 1
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
          RAW_TAG: ${{ needs.build.outputs.tag }}
          EXPECTED_TAG_OBJECT: ${{ needs.build.outputs.tag_object }}
        run: |
          set -euo pipefail
          actual_tag_object=$(gh api "repos/${GH_REPO}/git/ref/tags/${RAW_TAG}" --jq .object.sha)
          test "$actual_tag_object" = "$EXPECTED_TAG_OBJECT"
          gh release create "$RAW_TAG" \
            "release-artifacts/zai-coding-plan-v${VERSION}.so" \
            "release-artifacts/zai-coding-plan_${VERSION}_linux_amd64.zip" \
            release-artifacts/checksums.txt \
            release-artifacts/compatibility-evidence.json \
            release-artifacts/config.yaml.tmpl \
            release-artifacts/router-capacity-source.json \
            release-artifacts/verify-live.sh \
            release-artifacts/prepare-usage-dir.py \
            release-artifacts/remove-usage-output.py \
            release-artifacts/README.md \
            release-artifacts/release-sha.txt \
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
	matrix := map[string]any{
		"repository": hostImageRepository,
		"platform":   "linux/amd64",
		"baseline": map[string]string{
			"tag":             baselineHostImageTag,
			"manifest_digest": baselineHostImageAMD64Digest,
		},
	}
	matrixRaw, err := json.Marshal(matrix)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".github", "host-images.json"), matrixRaw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "deploy"), 0o755); err != nil {
		t.Fatal(err)
	}
	deployed := map[string]string{
		"repository":      hostImageRepository,
		"tag":             "v7.2.151",
		"platform":        "linux/amd64",
		"manifest_digest": "sha256:" + strings.Repeat("1", 64),
	}
	deployedRaw, err := json.Marshal(deployed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "deploy", "deployed-host-image.json"), deployedRaw, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(root, ".github", "scripts", "resolve-host-images.sh"),
		filepath.Join(root, ".github", "scripts", "run-host-matrix.sh"),
		filepath.Join(root, ".github", "workflows", "host-compatibility.yml"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "LICENSE"), []byte(canonicalMITLicense), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "NOTICE"), []byte(canonicalNotice), 0o644); err != nil {
		t.Fatal(err)
	}
	changelog := "## [0.2.0] - 2026-09-08\n\n[0.2.0]: https://github.com/TogetherWeOwn/cpa-plugin-zai-coding-plan/releases/tag/v0.2.0\n"
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
