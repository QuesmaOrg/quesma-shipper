package upload

import (
	"net/url"
	"strings"
	"testing"
)

// The exact-key check is a byte comparison, so the encoder must produce the fixture's spelling.
func TestCanonicalPathReproducesGoldenTicketPath(t *testing.T) {
	prepared, ticket := goldenPair(t, "request.json", "response.json")

	parsed, err := url.Parse(ticket.URL)
	if err != nil {
		t.Fatalf("parse golden ticket url: %v", err)
	}
	want := parsed.EscapedPath()
	if got := "/" + canonicalPath(prepared.Key); got != want {
		t.Fatalf("canonicalPath produced %q, golden ticket path is %q", got, want)
	}

	// Why hand-rolled: net/url leaves "=" unescaped and every mirror key carries organization=.
	if stdlib := (&url.URL{Path: "/" + prepared.Key}).EscapedPath(); stdlib == want {
		t.Fatalf("net/url now escapes this key grammar (%q): the hand-rolled encoder can go", stdlib)
	}
}

func TestCanonicalPathEscaping(t *testing.T) {
	cases := []struct {
		key  string
		want string
	}{
		{"organization=acme", "organization%3Dacme"},
		{"a/b/c.age", "a/b/c.age"},
		{"keep-._~", "keep-._~"},
		{"space here", "space%20here"},
		{"plus+sign", "plus%2Bsign"},
		{"percent%41", "percent%2541"},
		{"colon:slash?query", "colon%3Aslash%3Fquery"},
		{"café", "caf%C3%A9"},
	}
	for _, c := range cases {
		if got := canonicalPath(c.key); got != c.want {
			t.Errorf("canonicalPath(%q) = %q, want %q", c.key, got, c.want)
		}
	}
}

func TestCanonicalPathUsesUppercaseHex(t *testing.T) {
	got := canonicalPath("=")
	if got != "%3D" {
		t.Fatalf("canonicalPath(\"=\") = %q, want %%3D", got)
	}
	if strings.ContainsAny(got, "abcdef") {
		t.Fatalf("canonicalPath produced lowercase hex: %q", got)
	}
}

func TestValidateKeyRejects(t *testing.T) {
	cases := map[string]string{
		"empty":          "",
		"leading slash":  "/v1/object.age",
		"trailing slash": "v1/object.age/",
		"empty segment":  "v1//object.age",
		"dot segment":    "v1/./object.age",
		"dotdot segment": "v1/../object.age",
		"backslash":      `v1\object.age`,
		"control byte":   "v1/object\n.age",
		"too long":       strings.Repeat("a", maxKeyLength+1),
	}
	for name, key := range cases {
		if err := validateKey(key); err == nil {
			t.Errorf("%s: validateKey accepted %q", name, key)
		}
	}
	if err := validateKey("v1/organization=acme/mirror/object.age"); err != nil {
		t.Fatalf("validateKey refused a canonical key: %v", err)
	}
}
