//go:build !linux

package main

import (
	"fmt"
	"os"
)

const syscallNoFollow = 0

func openSecureDirectory(path string) (*os.File, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open secure store directory: %w", err)
	}
	return file, nil
}

func writeJSONAt(_ *os.File, name string, _ []byte) error {
	return fmt.Errorf("secure atomic replacement for %s is unsupported on this platform", name)
}
