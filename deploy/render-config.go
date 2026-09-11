package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
)

const (
	maxConfigBytes = 8 << 20
	maxSecretBytes = 64 << 10
)

func main() {
	if len(os.Args) != 4 {
		fatal("usage: render-config <base-config> <overlay-template> <candidate>")
	}
	planKey, err := readSecret("ZAI_CODING_PLAN_KEY_FILE")
	if err != nil {
		fatal(err.Error())
	}
	keySuffix, err := readSecret("ZAI_CODING_PLAN_KEY_SUFFIX_FILE")
	if err != nil {
		fatal(err.Error())
	}
	configDirectory, baseName, candidateName, err := openConfigDirectory(os.Args[1], os.Args[3])
	if err != nil {
		fatal(err.Error())
	}
	base, err := readDocumentAt(configDirectory, baseName)
	if err != nil {
		closeDirectory(configDirectory)
		fatal("base config: " + err.Error())
	}
	overlay, err := readDocument(os.Args[2])
	if err != nil {
		closeDirectory(configDirectory)
		fatal("overlay config: " + err.Error())
	}
	if err := substitute(overlay, map[string]string{
		"${ZAI_CODING_PLAN_KEY}":        planKey,
		"${ZAI_CODING_PLAN_KEY_SUFFIX}": keySuffix,
	}); err != nil {
		closeDirectory(configDirectory)
		fatal(err.Error())
	}
	merged, err := mergeDocuments(base, overlay)
	if err != nil {
		closeDirectory(configDirectory)
		fatal(err.Error())
	}
	if err := writeDocumentAt(configDirectory, candidateName, merged); err != nil {
		closeDirectory(configDirectory)
		fatal(err.Error())
	}
	if err := configDirectory.Close(); err != nil {
		fatal("config directory could not be closed")
	}
}

func fatal(message string) {
	fmt.Fprintln(os.Stderr, "render-config:", message)
	os.Exit(1)
}

func closeDirectory(directory *os.File) {
	if err := directory.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "render-config: config directory could not be closed")
	}
}

func readSecret(variable string) (string, error) {
	path := os.Getenv(variable)
	if path == "" {
		return "", fmt.Errorf("%s must name a root-owned 0600 file", variable)
	}
	file, err := openFileNoFollow(path, unix.O_RDONLY)
	if err != nil {
		return "", fmt.Errorf("%s could not be read", variable)
	}
	raw, info, err := readBoundedFile(file, maxSecretBytes)
	if err != nil {
		return "", fmt.Errorf("%s could not be read", variable)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return "", fmt.Errorf("%s must be a regular mode-0600 file owned by the effective user", variable)
	}
	value := strings.TrimSuffix(string(raw), "\n")
	if value == "" || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n") {
		return "", fmt.Errorf("%s must contain exactly one non-empty unpadded line", variable)
	}
	return value, nil
}

func readBoundedFile(file *os.File, limit int64) (raw []byte, info os.FileInfo, err error) {
	defer func() {
		if closeErr := file.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
	}()
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, info, errors.New("file is not a bounded regular file")
	}
	raw, err = io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, info, errors.New("file could not be read within its size limit")
	}
	return raw, info, nil
}

func openConfigDirectory(basePath, candidatePath string) (*os.File, string, string, error) {
	basePath, err := cleanAbsolutePath(basePath)
	if err != nil {
		return nil, "", "", errors.New("base config path must be absolute")
	}
	candidatePath, err = cleanAbsolutePath(candidatePath)
	if err != nil {
		return nil, "", "", errors.New("candidate config path must be absolute")
	}
	if filepath.Dir(basePath) != filepath.Dir(candidatePath) {
		return nil, "", "", errors.New("base and candidate configs must share one directory")
	}
	if filepath.Base(basePath) == filepath.Base(candidatePath) {
		return nil, "", "", errors.New("candidate config must differ from the base config")
	}
	directory, err := openDirectoryNoFollow(filepath.Dir(basePath))
	if err != nil {
		return nil, "", "", errors.New("config directory could not be opened securely")
	}
	info, err := directory.Stat()
	if err != nil {
		closeDirectory(directory)
		return nil, "", "", errors.New("config directory could not be inspected securely")
	}
	statValue, ok := info.Sys().(*syscall.Stat_t)
	if !ok || statValue.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0o077 != 0 {
		closeDirectory(directory)
		return nil, "", "", errors.New("config directory must be owned by the effective user and inaccessible to group and other")
	}
	return directory, filepath.Base(basePath), filepath.Base(candidatePath), nil
}

func readDocument(path string) (*yaml.Node, error) {
	file, err := openFileNoFollow(path, unix.O_RDONLY)
	if err != nil {
		return nil, errors.New("could not be opened securely")
	}
	return decodeDocument(file)
}

func readDocumentAt(directory *os.File, name string) (*yaml.Node, error) {
	file, err := openRegularAt(int(directory.Fd()), name, unix.O_RDONLY)
	if err != nil {
		return nil, errors.New("could not be opened securely")
	}
	return decodeDocument(file)
}

func decodeDocument(file *os.File) (document *yaml.Node, err error) {
	defer func() {
		if closeErr := file.Close(); err == nil && closeErr != nil {
			err = errors.New("could not be closed")
		}
	}()
	decoder := yaml.NewDecoder(io.LimitReader(file, maxConfigBytes+1))
	document = &yaml.Node{}
	if err := decoder.Decode(document); err != nil {
		return nil, errors.New("is not valid YAML")
	}
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("must contain one non-empty mapping document")
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("must contain exactly one YAML document")
	}
	return document, nil
}

func cleanAbsolutePath(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("path is not absolute")
	}
	cleaned := filepath.Clean(path)
	if cleaned == string(filepath.Separator) {
		return "", errors.New("path has no final component")
	}
	return cleaned, nil
}

func openFileNoFollow(path string, flags int) (*os.File, error) {
	cleaned, err := cleanAbsolutePath(path)
	if err != nil {
		return nil, err
	}
	directory, err := openDirectoryNoFollow(filepath.Dir(cleaned))
	if err != nil {
		return nil, err
	}
	file, openErr := openRegularAt(int(directory.Fd()), filepath.Base(cleaned), flags)
	closeErr := directory.Close()
	if openErr != nil {
		return nil, openErr
	}
	if closeErr != nil {
		if err := file.Close(); err != nil {
			return nil, errors.Join(closeErr, err)
		}
		return nil, closeErr
	}
	return file, nil
}

func openDirectoryNoFollow(path string) (*os.File, error) {
	cleaned, err := cleanAbsolutePath(filepath.Join(path, ".placeholder"))
	if err != nil {
		return nil, err
	}
	cleaned = filepath.Dir(cleaned)
	fd, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	for _, component := range strings.Split(strings.TrimPrefix(cleaned, string(filepath.Separator)), string(filepath.Separator)) {
		if component == "" {
			continue
		}
		nextFD, openErr := unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		closeErr := unix.Close(fd)
		if openErr != nil {
			return nil, openErr
		}
		if closeErr != nil {
			if err := unix.Close(nextFD); err != nil {
				return nil, errors.Join(closeErr, err)
			}
			return nil, closeErr
		}
		fd = nextFD
	}
	return os.NewFile(uintptr(fd), cleaned), nil
}

func openRegularAt(directoryFD int, name string, flags int) (*os.File, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, filepath.Separator) {
		return nil, errors.New("invalid final path component")
	}
	fd, err := unix.Openat(directoryFD, name, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		if closeErr := file.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
		if err == nil {
			err = errors.New("path is not a regular file")
		}
		return nil, err
	}
	return file, nil
}

func substitute(node *yaml.Node, values map[string]string) error {
	if node.Kind == yaml.ScalarNode {
		for placeholder, value := range values {
			if node.Value == placeholder {
				node.Value = value
				return nil
			}
			if strings.Contains(node.Value, placeholder) {
				return errors.New("secret placeholders must occupy the complete YAML scalar")
			}
		}
		return nil
	}
	for _, child := range node.Content {
		if err := substitute(child, values); err != nil {
			return err
		}
	}
	return nil
}

func mergeDocuments(base, overlay *yaml.Node) (*yaml.Node, error) {
	merged := clone(base)
	if err := mergeMapping(merged.Content[0], overlay.Content[0], nil); err != nil {
		return nil, err
	}
	return merged, nil
}

func mergeMapping(target, overlay *yaml.Node, path []string) error {
	if target.Kind != yaml.MappingNode || overlay.Kind != yaml.MappingNode {
		return errors.New("top-level config values must be mappings")
	}
	for index := 0; index < len(overlay.Content); index += 2 {
		key := overlay.Content[index]
		value := overlay.Content[index+1]
		if key.Kind != yaml.ScalarNode || key.Value == "" {
			return errors.New("config mapping keys must be non-empty scalars")
		}
		childPath := append(append([]string{}, path...), key.Value)
		position := mappingIndex(target, key.Value)
		if position < 0 {
			target.Content = append(target.Content, clone(key), clone(value))
			continue
		}
		existing := target.Content[position+1]
		switch {
		case existing.Kind == yaml.MappingNode && value.Kind == yaml.MappingNode:
			if err := mergeMapping(existing, value, childPath); err != nil {
				return err
			}
		case existing.Kind == yaml.SequenceNode && value.Kind == yaml.SequenceNode && preservesSequence(childPath):
			mergeSequence(existing, value, childPath)
		default:
			target.Content[position+1] = clone(value)
		}
	}
	return nil
}

func preservesSequence(path []string) bool {
	switch strings.Join(path, ".") {
	case "claude-api-key", "openai-compatibility", "plugins.store-sources":
		return true
	default:
		return false
	}
}

func mergeSequence(target, overlay *yaml.Node, path []string) {
	for _, item := range overlay.Content {
		identity := sequenceIdentity(path, item)
		replaced := false
		for index, existing := range target.Content {
			if identity != "" && sequenceIdentity(path, existing) == identity {
				target.Content[index] = clone(item)
				replaced = true
				break
			}
			if identity == "" && nodesEqual(existing, item) {
				replaced = true
				break
			}
		}
		if !replaced {
			target.Content = append(target.Content, clone(item))
		}
	}
}

func sequenceIdentity(path []string, node *yaml.Node) string {
	switch strings.Join(path, ".") {
	case "claude-api-key":
		if value := mappingScalar(node, "prefix"); value != "" {
			return "prefix:" + value
		}
		if value := mappingScalar(node, "base-url"); value != "" {
			return "base-url:" + value
		}
	case "openai-compatibility":
		if value := mappingScalar(node, "name"); value != "" {
			return "name:" + value
		}
	case "plugins.store-sources":
		if node.Kind == yaml.ScalarNode {
			return "source:" + node.Value
		}
	}
	return ""
}

func mappingScalar(node *yaml.Node, key string) string {
	if node.Kind != yaml.MappingNode {
		return ""
	}
	position := mappingIndex(node, key)
	if position < 0 || node.Content[position+1].Kind != yaml.ScalarNode {
		return ""
	}
	return node.Content[position+1].Value
}

func nodesEqual(left, right *yaml.Node) bool {
	if left.Kind != right.Kind || left.Tag != right.Tag || left.Value != right.Value || len(left.Content) != len(right.Content) {
		return false
	}
	for index := range left.Content {
		if !nodesEqual(left.Content[index], right.Content[index]) {
			return false
		}
	}
	return true
}

func mappingIndex(mapping *yaml.Node, key string) int {
	for index := 0; index < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			return index
		}
	}
	return -1
}

func clone(node *yaml.Node) *yaml.Node {
	copyNode := *node
	copyNode.Content = make([]*yaml.Node, len(node.Content))
	for index, child := range node.Content {
		copyNode.Content[index] = clone(child)
	}
	return &copyNode
}

func writeDocumentAt(directory *os.File, name string, document *yaml.Node) error {
	var buffer bytes.Buffer
	encoder := yaml.NewEncoder(&buffer)
	encoder.SetIndent(2)
	if err := encoder.Encode(document); err != nil {
		return errors.New("candidate could not be encoded")
	}
	if err := encoder.Close(); err != nil {
		return errors.New("candidate could not be encoded")
	}
	if buffer.Len() == 0 || buffer.Len() > maxConfigBytes {
		return errors.New("candidate config is empty or too large")
	}
	file, err := openRegularAt(int(directory.Fd()), name, unix.O_WRONLY)
	if err != nil {
		return errors.New("candidate config could not be opened securely")
	}
	info, err := file.Stat()
	if err != nil || info.Mode().Perm() != 0o600 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		if closeErr := file.Close(); closeErr != nil {
			return errors.New("candidate config could not be closed")
		}
		return errors.New("candidate config must be a mode-0600 regular file owned by the effective user")
	}
	if err := unix.Ftruncate(int(file.Fd()), 0); err != nil {
		if closeErr := file.Close(); closeErr != nil {
			return errors.New("candidate config could not be truncated or closed")
		}
		return errors.New("candidate config could not be truncated")
	}
	if _, err := file.Write(buffer.Bytes()); err != nil {
		if closeErr := file.Close(); closeErr != nil {
			return errors.New("candidate config could not be written or closed")
		}
		return errors.New("candidate config could not be written")
	}
	if err := file.Sync(); err != nil {
		if closeErr := file.Close(); closeErr != nil {
			return errors.New("candidate config could not be synced or closed")
		}
		return errors.New("candidate config could not be synced")
	}
	if err := file.Close(); err != nil {
		return errors.New("candidate config could not be closed")
	}
	return nil
}
