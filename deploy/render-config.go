package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

const maxConfigBytes = 8 << 20

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
	base, err := readDocument(os.Args[1])
	if err != nil {
		fatal("base config: " + err.Error())
	}
	overlay, err := readDocument(os.Args[2])
	if err != nil {
		fatal("overlay config: " + err.Error())
	}
	if err := substitute(overlay, map[string]string{
		"${ZAI_CODING_PLAN_KEY}":        planKey,
		"${ZAI_CODING_PLAN_KEY_SUFFIX}": keySuffix,
	}); err != nil {
		fatal(err.Error())
	}
	merged, err := mergeDocuments(base, overlay)
	if err != nil {
		fatal(err.Error())
	}
	if err := writeDocument(os.Args[3], merged); err != nil {
		fatal(err.Error())
	}
}

func fatal(message string) {
	fmt.Fprintln(os.Stderr, "render-config:", message)
	os.Exit(1)
}

func readSecret(variable string) (string, error) {
	path := os.Getenv(variable)
	if path == "" {
		return "", fmt.Errorf("%s must name a root-readable 0600 file", variable)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("%s could not be read", variable)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return "", fmt.Errorf("%s must be a regular mode-0600 file", variable)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s could not be read", variable)
	}
	value := strings.TrimSuffix(string(raw), "\n")
	if value == "" || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n") {
		return "", fmt.Errorf("%s must contain exactly one non-empty unpadded line", variable)
	}
	return value, nil
}

func readDocument(path string) (*yaml.Node, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("could not be opened")
	}
	defer file.Close()
	decoder := yaml.NewDecoder(io.LimitReader(file, maxConfigBytes+1))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return nil, errors.New("is not valid YAML")
	}
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("must contain one non-empty mapping document")
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("must contain exactly one YAML document")
	}
	return &document, nil
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
	if err := mergeMapping(merged.Content[0], overlay.Content[0]); err != nil {
		return nil, err
	}
	return merged, nil
}

func mergeMapping(target, overlay *yaml.Node) error {
	if target.Kind != yaml.MappingNode || overlay.Kind != yaml.MappingNode {
		return errors.New("top-level config values must be mappings")
	}
	for index := 0; index < len(overlay.Content); index += 2 {
		key := overlay.Content[index]
		value := overlay.Content[index+1]
		if key.Kind != yaml.ScalarNode || key.Value == "" {
			return errors.New("config mapping keys must be non-empty scalars")
		}
		position := mappingIndex(target, key.Value)
		if position < 0 {
			target.Content = append(target.Content, clone(key), clone(value))
			continue
		}
		existing := target.Content[position+1]
		if existing.Kind == yaml.MappingNode && value.Kind == yaml.MappingNode {
			if err := mergeMapping(existing, value); err != nil {
				return err
			}
			continue
		}
		target.Content[position+1] = clone(value)
	}
	return nil
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

func writeDocument(path string, document *yaml.Node) error {
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
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return errors.New("candidate config could not be opened")
	}
	if _, err := file.Write(buffer.Bytes()); err != nil {
		file.Close()
		return errors.New("candidate config could not be written")
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return errors.New("candidate config could not be synced")
	}
	if err := file.Close(); err != nil {
		return errors.New("candidate config could not be closed")
	}
	return nil
}
