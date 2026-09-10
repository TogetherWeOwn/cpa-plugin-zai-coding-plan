// release-validation validates release metadata, artifacts, documentation, and source files.
package main

import (
	"archive/zip"
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	pluginID             = "zai-coding-plan"
	libraryName          = pluginID + ".so"
	hostImageRepository  = "eceasy/cli-proxy-api"
	hostImageTag         = "v7.2.67"
	hostImageAMD64Digest = "sha256:49a249ba0cb867d2e70ef90f23d5fa8b6e2d04bf6c73d9e666e8eee8c353b606"
)

var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

const canonicalMITLicense = `MIT License

Copyright (c) 2026 TogetherWeOwn

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
`

const canonicalNotice = `cpa-plugin-zai-coding-plan
Copyright (c) 2026 TogetherWeOwn

This product includes software developed by TogetherWeOwn and third-party software whose licenses are recorded in the Go module dependency metadata.
`

type registry struct {
	Plugins []registryPlugin `json:"plugins"`
}

type registryPlugin struct {
	ID      string          `json:"id"`
	Version string          `json:"version"`
	License string          `json:"license"`
	Release registryRelease `json:"release"`
}

type registryRelease struct {
	Archive   string `json:"archive"`
	Checksums string `json:"checksums"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("release-validation", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	mode := flags.String("mode", "release", "validation mode: release, source, or version")
	root := flags.String("root", "", "repository root; defaults to the current directory")
	tag := flags.String("tag", "", "release tag, including the v prefix")
	version := flags.String("version", "", "release version without the v prefix")
	dist := flags.String("dist", "dist", "release artifact directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if *root == "" {
		workingDirectory, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("get current directory: %w", err)
		}
		*root = workingDirectory
	}

	switch *mode {
	case "source":
		return validateSource(*root)
	case "release":
		return validateRelease(*root, *dist, *tag, *version)
	case "version":
		resolved, err := versionFromTag(*tag)
		if err != nil {
			return err
		}
		fmt.Println(resolved)
		return nil
	default:
		return fmt.Errorf("unknown mode %q", *mode)
	}
}

func validateSource(root string) error {
	if err := validateDocumentation(root); err != nil {
		return err
	}
	if err := validateHostImagePin(root); err != nil {
		return err
	}
	if err := validateWorkflowActionPins(root); err != nil {
		return err
	}
	files, err := trackedFiles(root)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return errors.New("source validation found no files")
	}
	return nil
}

func validateRelease(root, dist, tag, version string) error {
	if err := validateVersion(tag, version); err != nil {
		return err
	}
	if err := validateDocumentation(root); err != nil {
		return err
	}
	if err := validateRegistry(root, version); err != nil {
		return err
	}
	if err := validateChangelog(root, version); err != nil {
		return err
	}

	distPath := resolvePath(root, dist)
	libraryPath := filepath.Join(distPath, fmt.Sprintf("%s-v%s.so", pluginID, version))
	archivePath := filepath.Join(distPath, fmt.Sprintf("%s_%s_linux_amd64.zip", pluginID, version))
	checksumsPath := filepath.Join(distPath, "checksums.txt")
	if err := requireNonEmpty(libraryPath); err != nil {
		return err
	}
	if err := requireNonEmpty(archivePath); err != nil {
		return err
	}
	if err := validateArchive(libraryPath, archivePath); err != nil {
		return err
	}
	return validateChecksums(checksumsPath, []string{libraryPath, archivePath})
}

func validateVersion(tag, version string) error {
	resolved, err := versionFromTag(tag)
	if err != nil {
		return err
	}
	if resolved != version {
		return fmt.Errorf("tag %q does not match version %q", tag, version)
	}
	return nil
}

func versionFromTag(tag string) (string, error) {
	if !strings.HasPrefix(tag, "v") {
		return "", fmt.Errorf("tag %q must start with v", tag)
	}
	version := strings.TrimPrefix(tag, "v")
	if !versionPattern.MatchString(version) {
		return "", fmt.Errorf("tag %q is not valid semantic version syntax", tag)
	}
	return version, nil
}

func validateRegistry(root, version string) error {
	path := filepath.Join(root, "registry.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read registry: %w", err)
	}
	var value registry
	if err := json.Unmarshal(raw, &value); err != nil {
		return fmt.Errorf("parse registry: %w", err)
	}
	matches := make([]registryPlugin, 0, 1)
	for _, plugin := range value.Plugins {
		if plugin.ID == pluginID {
			matches = append(matches, plugin)
		}
	}
	if len(matches) != 1 {
		return fmt.Errorf("registry must contain exactly one %q entry, found %d", pluginID, len(matches))
	}
	if matches[0].Version != version {
		return fmt.Errorf("registry version %q does not match release version %q", matches[0].Version, version)
	}
	if matches[0].License != "MIT" {
		return fmt.Errorf("registry license %q does not match LICENSE", matches[0].License)
	}
	wantArchive := fmt.Sprintf("%s_%s_linux_amd64.zip", pluginID, version)
	if matches[0].Release.Archive != wantArchive {
		return fmt.Errorf("registry archive %q does not match release archive %q", matches[0].Release.Archive, wantArchive)
	}
	if matches[0].Release.Checksums != "checksums.txt" {
		return fmt.Errorf("registry checksums %q must be checksums.txt", matches[0].Release.Checksums)
	}
	return nil
}

func validateChangelog(root, version string) error {
	raw, err := os.ReadFile(filepath.Join(root, "CHANGELOG.md"))
	if err != nil {
		return fmt.Errorf("read changelog: %w", err)
	}
	text := string(raw)
	headingPattern := regexp.MustCompile(`(?m)^## \[` + regexp.QuoteMeta(version) + `\] - (\d{4}-\d{2}-\d{2})$`)
	if !headingPattern.MatchString(text) {
		return fmt.Errorf("changelog must contain a dated [%s] release heading", version)
	}
	link := fmt.Sprintf("[%s]: https://github.com/TogetherWeOwn/cpa-plugin-zai-coding-plan/releases/tag/v%s", version, version)
	if !strings.Contains(text, link) {
		return fmt.Errorf("changelog is missing the %s release link", version)
	}
	return nil
}

func validateDocumentation(root string) error {
	license, err := os.ReadFile(filepath.Join(root, "LICENSE"))
	if err != nil {
		return fmt.Errorf("read LICENSE: %w", err)
	}
	if normalizeDocument(string(license)) != normalizeDocument(canonicalMITLicense) {
		return errors.New("LICENSE does not match the canonical MIT license")
	}

	notice, err := os.ReadFile(filepath.Join(root, "NOTICE"))
	if err != nil {
		return fmt.Errorf("read NOTICE: %w", err)
	}
	if normalizeDocument(string(notice)) != normalizeDocument(canonicalNotice) {
		return errors.New("NOTICE does not match the canonical project notice")
	}
	return nil
}

func normalizeDocument(value string) string {
	return strings.TrimSpace(strings.ReplaceAll(value, "\r\n", "\n"))
}

func validateHostImagePin(root string) error {
	path := filepath.Join(root, ".github", "release-host-image.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read release host image pin: %w", err)
	}
	var pin struct {
		Repository     string `json:"repository"`
		Tag            string `json:"tag"`
		Platform       string `json:"platform"`
		ManifestDigest string `json:"manifest_digest"`
	}
	if err := json.Unmarshal(raw, &pin); err != nil {
		return fmt.Errorf("parse release host image pin: %w", err)
	}
	if pin.Repository != hostImageRepository || pin.Tag != hostImageTag || pin.Platform != "linux/amd64" || pin.ManifestDigest != hostImageAMD64Digest {
		return fmt.Errorf("release host image pin does not match the approved v7.2.67 linux/amd64 manifest")
	}
	return nil
}

func validateWorkflowActionPins(root string) error {
	paths, err := filepath.Glob(filepath.Join(root, ".github", "workflows", "*.yml"))
	if err != nil {
		return fmt.Errorf("list workflows: %w", err)
	}
	if len(paths) == 0 {
		return errors.New("no GitHub workflows found")
	}
	pinned := regexp.MustCompile(`^[0-9a-f]{40}$`)
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read workflow %s: %w", filepath.Base(path), err)
		}
		for index, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "uses:") && !strings.HasPrefix(trimmed, "- uses:") {
				continue
			}
			value := strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))
			value = strings.TrimSpace(strings.TrimPrefix(value, "uses:"))
			if strings.HasPrefix(value, "./") {
				continue
			}
			parts := strings.Split(value, "@")
			if len(parts) != 2 || !pinned.MatchString(strings.Fields(parts[1])[0]) {
				return fmt.Errorf("workflow %s:%d action is not pinned to a full commit SHA", filepath.Base(path), index+1)
			}
		}
	}
	return nil
}

func trackedFiles(root string) ([]string, error) {
	// Git's path list is the deterministic source of files to scan. It includes
	// tracked and untracked, non-ignored files while excluding generated output.
	command := exec.Command("git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	if len(output) == 0 {
		return nil, nil
	}
	parts := strings.Split(string(output[:len(output)-1]), "\x00")
	files := make([]string, 0, len(parts))
	for _, name := range parts {
		if name != "" {
			files = append(files, name)
		}
	}
	return files, nil
}

func requireNonEmpty(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("required artifact %s: %w", filepath.Base(path), err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return fmt.Errorf("required artifact %s is empty or not a regular file", filepath.Base(path))
	}
	return nil
}

func validateArchive(libraryPath, archivePath string) error {
	archive, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	defer archive.Close()
	if len(archive.File) != 1 || archive.File[0].Name != libraryName {
		entries := make([]string, 0, len(archive.File))
		for _, entry := range archive.File {
			entries = append(entries, entry.Name)
		}
		return fmt.Errorf("archive entries %q, want exactly %q", entries, libraryName)
	}
	entry, err := archive.File[0].Open()
	if err != nil {
		return fmt.Errorf("open archive entry: %w", err)
	}
	defer entry.Close()
	library, err := os.Open(libraryPath)
	if err != nil {
		return fmt.Errorf("open library: %w", err)
	}
	defer library.Close()
	equal, err := readersEqual(library, entry)
	if err != nil {
		return fmt.Errorf("compare archive library: %w", err)
	}
	if !equal {
		return errors.New("archive library does not match versioned shared library")
	}
	return nil
}

func readersEqual(left, right io.Reader) (bool, error) {
	leftHash := sha256.New()
	rightHash := sha256.New()
	if _, err := io.Copy(leftHash, left); err != nil {
		return false, err
	}
	if _, err := io.Copy(rightHash, right); err != nil {
		return false, err
	}
	return string(leftHash.Sum(nil)) == string(rightHash.Sum(nil)), nil
}

func validateChecksums(path string, artifactPaths []string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open checksums: %w", err)
	}
	defer file.Close()

	checksums := make(map[string]string)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Fields(line)
		if len(parts) != 2 || len(parts[0]) != sha256.Size*2 {
			return fmt.Errorf("invalid checksum line %q", line)
		}
		if _, err := hex.DecodeString(parts[0]); err != nil {
			return fmt.Errorf("invalid checksum for %s: %w", parts[1], err)
		}
		name := strings.TrimPrefix(parts[1], "*")
		if filepath.Base(name) != name {
			return fmt.Errorf("checksum name %q must be a base filename", name)
		}
		if _, exists := checksums[name]; exists {
			return fmt.Errorf("duplicate checksum for %s", name)
		}
		checksums[name] = strings.ToLower(parts[0])
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read checksums: %w", err)
	}
	if len(checksums) != len(artifactPaths) {
		return fmt.Errorf("checksums contains %d entries, want %d", len(checksums), len(artifactPaths))
	}
	for _, artifactPath := range artifactPaths {
		name := filepath.Base(artifactPath)
		want, exists := checksums[name]
		if !exists {
			return fmt.Errorf("checksums missing %s", name)
		}
		got, err := fileSHA256(artifactPath)
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("checksum mismatch for %s", name)
		}
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s for checksum: %w", filepath.Base(path), err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("checksum %s: %w", filepath.Base(path), err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func resolvePath(root, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(root, path)
}
