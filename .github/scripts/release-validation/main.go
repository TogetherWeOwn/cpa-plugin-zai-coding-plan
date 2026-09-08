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
	"sort"
	"strings"
)

const (
	pluginID    = "zai-coding-plan"
	libraryName = pluginID + ".so"
)

var (
	versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)
	secretPatterns = []struct {
		name    string
		pattern *regexp.Regexp
	}{
		{name: "private key", pattern: regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ )?PRIVATE KEY-----`)},
		{name: "GitHub token", pattern: regexp.MustCompile(`\bgh[opusr]_[A-Za-z0-9]{36,255}\b`)},
		{name: "AWS access key", pattern: regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`)},
		{name: "Google API key", pattern: regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)},
		{name: "Slack token", pattern: regexp.MustCompile(`\bxox[baprs]-[0-9A-Za-z-]{10,}\b`)},
	}
)

type registry struct {
	Plugins []registryPlugin `json:"plugins"`
}

type registryPlugin struct {
	ID      string           `json:"id"`
	Version string           `json:"version"`
	License string           `json:"license"`
	Release *registryRelease `json:"release,omitempty"`
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
	mode := flags.String("mode", "release", "validation mode: release or source")
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
	default:
		return fmt.Errorf("unknown mode %q", *mode)
	}
}

func validateSource(root string) error {
	if err := validateDocumentation(root); err != nil {
		return err
	}
	files, err := trackedFiles(root)
	if err != nil {
		return err
	}
	return scanSecrets(root, files)
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
	if !versionPattern.MatchString(version) {
		return fmt.Errorf("version %q is not valid semantic version syntax", version)
	}
	if tag != "v"+version {
		return fmt.Errorf("tag %q does not match version %q", tag, version)
	}
	return nil
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
	if matches[0].Release != nil {
		wantArchive := fmt.Sprintf("%s_%s_linux_amd64.zip", pluginID, version)
		if matches[0].Release.Archive != wantArchive {
			return fmt.Errorf("registry archive %q does not match release archive %q", matches[0].Release.Archive, wantArchive)
		}
		if matches[0].Release.Checksums != "checksums.txt" {
			return fmt.Errorf("registry checksums %q must be checksums.txt", matches[0].Release.Checksums)
		}
	}
	return nil
}

func validateDocumentation(root string) error {
	licensePath := filepath.Join(root, "LICENSE")
	license, err := os.ReadFile(licensePath)
	if err != nil {
		return fmt.Errorf("read LICENSE: %w", err)
	}
	licenseText := string(license)
	if !strings.Contains(licenseText, "MIT License") || !strings.Contains(licenseText, "Permission is hereby granted") {
		return errors.New("LICENSE does not contain the expected MIT license text")
	}

	noticePath := filepath.Join(root, "NOTICE")
	notice, err := os.ReadFile(noticePath)
	if err != nil {
		return fmt.Errorf("read NOTICE: %w", err)
	}
	if len(strings.TrimSpace(string(notice))) == 0 {
		return errors.New("NOTICE is empty")
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

func scanSecrets(root string, files []string) error {
	if len(files) == 0 {
		return errors.New("secret scan found no files to scan")
	}
	sort.Strings(files)
	scanned := 0
	for _, name := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("stat secret-scan file %s: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read secret-scan file %s: %w", name, err)
		}
		if isBinary(raw) {
			continue
		}
		scanned++
		for _, secret := range secretPatterns {
			location := secret.pattern.FindIndex(raw)
			if location != nil {
				line := 1 + strings.Count(string(raw[:location[0]]), "\n")
				return fmt.Errorf("secret scan detected %s in %s:%d", secret.name, name, line)
			}
		}
	}
	if scanned == 0 {
		return errors.New("secret scan found no text files to scan")
	}
	fmt.Printf("secret scan: scanned %d text files\n", scanned)
	return nil
}

func isBinary(data []byte) bool {
	limit := len(data)
	if limit > 8*1024 {
		limit = 8 * 1024
	}
	return strings.IndexByte(string(data[:limit]), 0) >= 0
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
