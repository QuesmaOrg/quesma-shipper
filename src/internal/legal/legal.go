// Package legal embeds the license, the NOTICE file and every bundled dependency's license text,
// so a binary downloaded on its own still carries the notices its licenses require.
// `make licenses` regenerates third_party/ and refreshes the LICENSE and NOTICE copies.
package legal

import (
	"embed"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"
)

//go:embed LICENSE NOTICE third_party references
var FS embed.FS

// Write prints NOTICE, then LICENSE, then each third-party license text under a header naming the
// dependency, then the licenses of reference works that are cited but not bundled.
func Write(w io.Writer) error {
	for _, name := range []string{"NOTICE", "LICENSE"} {
		if err := section(w, name, name); err != nil {
			return err
		}
	}
	var paths []string
	if err := fs.WalkDir(FS, "third_party/licenses", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			paths = append(paths, p)
		}
		return err
	}); err != nil {
		return err
	}
	sort.Strings(paths)
	for _, p := range paths {
		dep := strings.TrimPrefix(strings.TrimSuffix(p, "/"+p[strings.LastIndex(p, "/")+1:]), "third_party/licenses/")
		if err := section(w, dep+" ("+p[strings.LastIndex(p, "/")+1:]+")", p); err != nil {
			return err
		}
	}
	return section(w, "gitleaks default ruleset (reference, not bundled)", "references/gitleaks/LICENSE")
}

func section(w io.Writer, title, path string) error {
	b, err := FS.ReadFile(path)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n%s\n\n%s\n\n", strings.Repeat("=", 72), title, strings.TrimRight(string(b), "\n"))
	return err
}
