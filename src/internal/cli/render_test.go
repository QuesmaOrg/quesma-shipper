package cli

import (
	"bytes"
	"github.com/QuesmaOrg/quesma-shipper/app"
	"strings"
	"testing"
)

// Columns align by rune count, not bytes, so a multi-byte label cannot shift them.
func TestRenderSectionsAlignment(t *testing.T) {
	var out bytes.Buffer
	renderSections(&out, palette{}, []app.Section{{
		Title: "Check",
		Rows: []app.Row{
			{Sev: app.SevOK, Label: "a", Detail: "first"},
			// 6 runes but 7 bytes: byte-counted padding would misalign every row after it.
			{Sev: app.SevWarn, Label: "länger", Detail: "second", Fix: "do the thing"},
			{Sev: app.SevDim, Label: "b", Detail: "third"},
		},
	}})

	want := "" +
		"Check\n" +
		"  ✓  a       first\n" +
		"  !  länger  second\n" +
		"     → do the thing\n" +
		"  -  b       third\n"
	if out.String() != want {
		t.Fatalf("layout drifted:\ngot:\n%q\nwant:\n%q", out.String(), want)
	}
}

func TestRenderSectionsUntitledAndMultilineFix(t *testing.T) {
	var out bytes.Buffer
	renderSections(&out, palette{}, []app.Section{{
		Rows: []app.Row{{Sev: app.SevWarn, Label: "parked", Detail: "2 file(s)", Fix: "one\ntwo"}},
	}})
	want := "" +
		"  !  parked  2 file(s)\n" +
		"     → one\n" +
		"       two\n"
	if out.String() != want {
		t.Fatalf("got:\n%q\nwant:\n%q", out.String(), want)
	}
}

func TestRenderSectionsColor(t *testing.T) {
	var out bytes.Buffer
	renderSections(&out, ansiPalette(), []app.Section{{
		Title: "T",
		Rows: []app.Row{
			{Sev: app.SevOK, Label: "fine", Detail: "yes"},
			{Sev: app.SevDim, Label: "ref", Detail: "detail"},
		},
	}})
	s := out.String()
	for _, want := range []string{
		"\x1b[32m✓\x1b[0m", // green glyph
		"\x1b[2mT\x1b[0m",  // dim title
		"\x1b[2m  -  ref",  // dim rows dim as a whole line
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%q", want, s)
		}
	}
}

// TestPaint pins which substrings read as values, and that a dim row restores its dim.
func TestPaint(t *testing.T) {
	p := ansiPalette()
	cases := []struct {
		in   string
		want []string // painted tokens
		not  []string // must stay unpainted
	}{
		{"1,021 sessions across 30 projects", []string{"1,021", "30"}, []string{"sessions", "projects"}},
		{"s3://bucket/path - write access verified", []string{"s3://bucket/path"}, []string{"write"}},
		{"quesma · control.example.com - connected", []string{"control.example.com"}, []string{"connected"}},
		{"v0.144.6 and 0.0.0-95c699c34d8b+dirty", []string{"v0.144.6"}, nil},
		{"9m30s ago, checked 2.4s", []string{"9m30s", "2.4s"}, []string{"ago"}},
		{"fetched 2026-08-18T16:31:52+02:00", []string{"2026-08-18T16:31:52+02:00"}, nil},
	}
	for _, c := range cases {
		got := p.paint(c.in, "")
		for _, tok := range c.want {
			if !strings.Contains(got, p.cyan+tok+p.reset) {
				t.Errorf("paint(%q): token %q not painted in %q", c.in, tok, got)
			}
		}
		for _, word := range c.not {
			if strings.Contains(got, p.cyan+word) {
				t.Errorf("paint(%q): prose %q wrongly painted", c.in, word)
			}
		}
	}

	if got := p.paint("12 files", p.dim); !strings.Contains(got, p.cyan+"12"+p.reset+p.dim) {
		t.Errorf("restore state not re-established after token: %q", got)
	}
	if got := (palette{}).paint("12 files", ""); got != "12 files" {
		t.Errorf("zero palette must be a no-op, got %q", got)
	}
}

func TestColorEnabled(t *testing.T) {
	env := func(vals map[string]string) func(string) string {
		return func(k string) string { return vals[k] }
	}
	cases := []struct {
		name string
		tty  bool
		vals map[string]string
		want bool
	}{
		{"piped", false, nil, false},
		{"tty", true, nil, true},
		{"no_color", true, map[string]string{"NO_COLOR": "1"}, false},
		{"dumb_term", true, map[string]string{"TERM": "dumb"}, false},
	}
	for _, c := range cases {
		if got := colorEnabled(c.tty, env(c.vals)); got != c.want {
			t.Errorf("%s: colorEnabled = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestVerdict(t *testing.T) {
	cases := []struct {
		fails, warns int
		want         string
	}{
		{0, 0, "Everything is being collected."},
		{0, 1, "1 issue needs attention."},
		{0, 3, "3 issues need attention."},
		{1, 0, "1 problem is stopping collection."},
		{2, 2, "2 problems are stopping collection; 2 issues need attention."},
	}
	for _, c := range cases {
		if got := verdict(c.fails, c.warns); got != c.want {
			t.Errorf("verdict(%d, %d) = %q, want %q", c.fails, c.warns, got, c.want)
		}
	}
}
