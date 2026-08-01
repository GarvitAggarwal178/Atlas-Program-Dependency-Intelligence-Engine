// Package modver parses a go.mod file's require directives into a
// module-path -> version map, the input architecture.md section 7's
// "Dependency version bump" delta needs to detect.
package modver

import (
	"fmt"
	"os"

	"golang.org/x/mod/modfile"
)

// ParseRequires reads gomodPath and returns every required module's
// version, keyed by module path. Indirect requires are included — a
// version bump anywhere in the dependency graph as recorded in go.mod is a
// real delta, whether the requirement is direct or transitive.
func ParseRequires(gomodPath string) (map[string]string, error) {
	data, err := os.ReadFile(gomodPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", gomodPath, err)
	}
	f, err := modfile.Parse(gomodPath, data, nil)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", gomodPath, err)
	}

	result := make(map[string]string, len(f.Require))
	for _, r := range f.Require {
		result[r.Mod.Path] = r.Mod.Version
	}
	return result, nil
}
