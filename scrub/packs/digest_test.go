package packs

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestDigestIgnoresCheckoutFormatting(t *testing.T) {
	for _, pack := range PatternPacks {
		t.Run(pack, func(t *testing.T) {
			raw, err := corpora.ReadFile("data/" + pack + ".json")
			if err != nil {
				t.Fatal(err)
			}
			want, err := Digest(pack)
			if err != nil {
				t.Fatal(err)
			}
			lf := bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
			var compact bytes.Buffer
			if err := json.Compact(&compact, lf); err != nil {
				t.Fatal(err)
			}
			var reordered map[string]json.RawMessage
			if err := json.Unmarshal(lf, &reordered); err != nil {
				t.Fatal(err)
			}
			reordered["description"] = json.RawMessage(`"documentation only"`)
			body, err := json.Marshal(reordered)
			if err != nil {
				t.Fatal(err)
			}
			for name, variant := range map[string][]byte{
				"LF":                                 lf,
				"CRLF":                               bytes.ReplaceAll(lf, []byte("\n"), []byte("\r\n")),
				"compact":                            compact.Bytes(),
				"reordered with changed description": body,
			} {
				if got, err := digestRules(variant); err != nil || got != want {
					t.Errorf("%s: digest=%s, err=%v; want %s", name, got, err, want)
				}
			}
			var changed corpus
			if err := json.Unmarshal(lf, &changed); err != nil {
				t.Fatal(err)
			}
			changed.Rules[0].Regex += "x"
			body, err = json.Marshal(changed)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := digestRules(body); err != nil || got == want {
				t.Errorf("changed rule: digest=%s, err=%v; must differ from %s", got, err, want)
			}
		})
	}
}

func TestDigestRejectsInvalidRules(t *testing.T) {
	if _, err := digestRules([]byte(`{"rules":`)); err == nil {
		t.Fatal("invalid JSON has a digest")
	}
	if _, err := Digest("missing-pack"); err == nil {
		t.Fatal("missing pack has a digest")
	}
}
