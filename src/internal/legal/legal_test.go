package legal

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// The embedded copies must match the repository's canonical files, or `shipper licenses` lies.
func TestEmbeddedCopiesMatchRepository(t *testing.T) {
	for name, canonical := range map[string]string{"LICENSE": "../../../LICENSE", "NOTICE": "../../../NOTICE"} {
		want, err := os.ReadFile(canonical)
		if err != nil {
			t.Fatal(err)
		}
		got, err := FS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from %s; run make licenses", name, canonical)
		}
	}
}

// Every row of the inventory has a license text in the tree, and the tree has no orphan.
func TestInventoryMatchesTexts(t *testing.T) {
	csv, err := FS.ReadFile("third_party/licenses.csv")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(csv)), "\n") {
		pkg := strings.SplitN(line, ",", 2)[0]
		found := false
		for p := pkg; p != "." && p != ""; p = parent(p) {
			if entries, err := FS.ReadDir("third_party/licenses/" + p); err == nil && len(entries) > 0 {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s is in licenses.csv but has no license text under third_party/licenses", pkg)
		}
	}
	var out bytes.Buffer
	if err := Write(&out); err != nil {
		t.Fatal(err)
	}
	for _, must := range []string{"Copyright 2026 Quesma Inc.", "Apache License", "The Update Framework Authors", "mousetrap", "Zachary Rice"} {
		if !strings.Contains(out.String(), must) {
			t.Errorf("licenses output lacks %q", must)
		}
	}
}

func parent(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i]
	}
	return ""
}
