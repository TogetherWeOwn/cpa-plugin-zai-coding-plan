package main

import (
	"archive/zip"
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type artifact struct {
	name string
	path string
}

func main() {
	libraryPath := flag.String("library", "", "path to the compiled plugin library")
	entryName := flag.String("entry", "", "dynamic library name inside the zip")
	archivePath := flag.String("archive", "", "path to the output zip archive")
	operatorPath := flag.String("operator", "", "path to the output operator zip archive")
	operatorManifestPath := flag.String("operator-manifest", "", "path to a newline-delimited name=path operator manifest")
	checksumPath := flag.String("checksum", "", "path to the output checksum file")
	checksumManifestPath := flag.String("checksum-manifest", "", "path to a newline-delimited name=path artifact manifest")
	flag.Parse()

	if *libraryPath == "" || *entryName == "" || *archivePath == "" || *operatorPath == "" || *operatorManifestPath == "" || *checksumPath == "" || *checksumManifestPath == "" {
		fatalf("library, entry, archive, operator, operator-manifest, checksum, and checksum-manifest are required")
	}
	if filepath.Base(*entryName) != *entryName {
		fatalf("entry must be a root-level filename")
	}
	if _, err := packageLibrary(*libraryPath, *entryName, *archivePath); err != nil {
		fatalf("%v", err)
	}
	operatorEntries, err := readManifest(*operatorManifestPath)
	if err != nil {
		fatalf("read operator manifest: %v", err)
	}
	if err := packageOperator(operatorEntries, *operatorPath); err != nil {
		fatalf("%v", err)
	}
	checksumEntries, err := readManifest(*checksumManifestPath)
	if err != nil {
		fatalf("read checksum manifest: %v", err)
	}
	checksumEntries = append(checksumEntries,
		artifact{name: filepath.Base(*libraryPath), path: *libraryPath},
		artifact{name: filepath.Base(*archivePath), path: *archivePath},
		artifact{name: filepath.Base(*operatorPath), path: *operatorPath},
	)
	if err := writeChecksums(*checksumPath, checksumEntries); err != nil {
		fatalf("%v", err)
	}
}

func readManifest(path string) ([]artifact, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var artifacts []artifact
	seen := make(map[string]struct{})
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.TrimSpace(line) != line || strings.HasPrefix(line, "#") {
			return nil, fmt.Errorf("invalid manifest line %q", line)
		}
		name, path, ok := strings.Cut(line, "=")
		if !ok || name == "" || path == "" || filepath.Base(name) != name {
			return nil, fmt.Errorf("invalid manifest line %q", line)
		}
		if _, exists := seen[name]; exists {
			return nil, fmt.Errorf("duplicate manifest entry %q", name)
		}
		seen[name] = struct{}{}
		artifacts = append(artifacts, artifact{name: name, path: path})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(artifacts) == 0 {
		return nil, fmt.Errorf("manifest is empty")
	}
	return artifacts, nil
}

func packageLibrary(libraryPath, entryName, archivePath string) ([]byte, error) {
	if err := writeZip(archivePath, []artifact{{name: entryName, path: libraryPath}}); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(archivePath)
	if err != nil {
		return nil, fmt.Errorf("read archive: %w", err)
	}
	return data, nil
}

func packageOperator(entries []artifact, archivePath string) error {
	return writeZip(archivePath, entries)
}

func writeZip(archivePath string, entries []artifact) error {
	if err := os.MkdirAll(filepath.Dir(archivePath), 0o755); err != nil {
		return fmt.Errorf("create archive directory: %w", err)
	}
	archive, err := os.Create(archivePath)
	if err != nil {
		return fmt.Errorf("create archive: %w", err)
	}
	archiveClosed := false
	defer func() {
		if !archiveClosed {
			_ = archive.Close()
		}
	}()

	writer := zip.NewWriter(archive)
	for _, item := range entries {
		if filepath.Base(item.name) != item.name {
			return fmt.Errorf("zip entry %q must be a root-level filename", item.name)
		}
		info, err := os.Stat(item.path)
		if err != nil {
			return fmt.Errorf("stat %s: %w", item.name, err)
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			return fmt.Errorf("zip entry %s is empty or not a regular file", item.name)
		}
		source, err := os.Open(item.path)
		if err != nil {
			return fmt.Errorf("open %s: %w", item.name, err)
		}
		header := &zip.FileHeader{Name: item.name, Method: zip.Deflate}
		header.SetMode(info.Mode().Perm())
		entry, err := writer.CreateHeader(header)
		if err != nil {
			_ = source.Close()
			return fmt.Errorf("create zip entry %s: %w", item.name, err)
		}
		if _, err := io.Copy(entry, source); err != nil {
			_ = source.Close()
			return fmt.Errorf("copy %s: %w", item.name, err)
		}
		if err := source.Close(); err != nil {
			return fmt.Errorf("close %s: %w", item.name, err)
		}
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("close zip writer: %w", err)
	}
	if err := archive.Close(); err != nil {
		return fmt.Errorf("close archive: %w", err)
	}
	archiveClosed = true
	return nil
}

func writeChecksums(path string, artifacts []artifact) error {
	seen := make(map[string]struct{}, len(artifacts))
	for _, item := range artifacts {
		if filepath.Base(item.name) != item.name {
			return fmt.Errorf("checksum name %q must be a base filename", item.name)
		}
		if _, exists := seen[item.name]; exists {
			return fmt.Errorf("duplicate checksum artifact %q", item.name)
		}
		seen[item.name] = struct{}{}
	}
	sort.Slice(artifacts, func(left, right int) bool { return artifacts[left].name < artifacts[right].name })
	var checksums strings.Builder
	for _, item := range artifacts {
		file, err := os.Open(item.path)
		if err != nil {
			return fmt.Errorf("open %s for checksum: %w", item.name, err)
		}
		hash := sha256.New()
		_, errCopy := io.Copy(hash, file)
		errClose := file.Close()
		if errCopy != nil {
			return fmt.Errorf("checksum %s: %w", item.name, errCopy)
		}
		if errClose != nil {
			return fmt.Errorf("close %s: %w", item.name, errClose)
		}
		checksums.WriteString(hex.EncodeToString(hash.Sum(nil)))
		checksums.WriteString("  ")
		checksums.WriteString(item.name)
		checksums.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(checksums.String()), 0o644); err != nil {
		return fmt.Errorf("write checksum: %w", err)
	}
	return nil
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
