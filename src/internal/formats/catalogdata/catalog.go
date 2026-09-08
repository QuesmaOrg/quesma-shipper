// Package catalogdata embeds the bundled source-spec files as data. It carries the scope
// ceiling: coverage inside a compiled root family is a data-file change, a new root needs a
// release. No resolution happens here (env, globs, deny lists); that is the config layer.
package catalogdata

import (
	"embed"
	"fmt"
	"io/fs"
	"slices"
)

//go:embed *.yaml
var FS embed.FS

// Files returns the embedded catalog file names, sorted.
func Files() ([]string, error) {
	entries, err := fs.ReadDir(FS, ".")
	if err != nil {
		return nil, fmt.Errorf("catalog: read embedded dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		names = append(names, e.Name())
	}
	slices.Sort(names)
	return names, nil
}

// Read returns the raw bytes of one embedded catalog file.
func Read(name string) ([]byte, error) {
	b, err := FS.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("catalog: read %s: %w", name, err)
	}
	return b, nil
}
