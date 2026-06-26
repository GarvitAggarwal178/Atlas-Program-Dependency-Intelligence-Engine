// Package modpath provides utilities for reading go.mod to extract the module
// path declared therein. This avoids an external dependency on golang.org/x/mod.
package modpath

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FindModulePath walks upward from startDir looking for a go.mod file and
// returns the module path declared in its first "module" directive.
// Returns ("", error) if no go.mod is found within the filesystem root.
func FindModulePath(startDir string) (repoRoot, modulePath string, err error) {
	dir := startDir
	for {
		candidate := filepath.Join(dir, "go.mod")
		if _, err := os.Stat(candidate); err == nil {
			path, err := readModulePath(candidate)
			if err != nil {
				return "", "", err
			}
			return dir, path, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break // reached filesystem root
		}
		dir = parent
	}
	return "", "", fmt.Errorf("no go.mod found above %s", startDir)
}

// readModulePath opens a go.mod file and returns the argument of the first
// "module" directive it finds.
func readModulePath(gomodPath string) (string, error) {
	f, err := os.Open(gomodPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "module ") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				return parts[1], nil
			}
		}
	}
	return "", fmt.Errorf("%s: no module directive found", gomodPath)
}
