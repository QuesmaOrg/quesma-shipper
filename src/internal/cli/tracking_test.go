package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/app"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
)

var testNow = time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)

func TestDecodeKey(t *testing.T) {
	for _, tc := range []struct {
		in   []byte
		want key
	}{
		{[]byte{0x1b, '[', 'A'}, keyUp}, {[]byte{0x1b, '[', 'B'}, keyDown},
		{[]byte{0x1b, '[', 'C'}, keyOpen}, {[]byte{0x1b, '[', 'D'}, keyBack},
		{[]byte("k"), keyUp}, {[]byte("j"), keyDown}, {[]byte("\r"), keyOpen}, {[]byte("b"), keyBack},
		{[]byte("t"), keyToggle}, {[]byte(" "), keyToggle}, {[]byte("q"), keyQuit}, {[]byte{0x03}, keyQuit},
		{[]byte("z"), keyNone}, {[]byte{0x1b, '[', 'Z'}, keyNone}, {nil, keyNone},
	} {
		if got := decodeKey(tc.in); got != tc.want {
			t.Errorf("decodeKey(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestCursorWrapsAndSurvivesAnEmptyLevel(t *testing.T) {
	b := &browser{rows: browseRows(t.TempDir())}
	b.move(-1)
	if b.sel != 1 {
		t.Errorf("up from the first row should wrap, got %d", b.sel)
	}
	b.move(1)
	if b.sel != 0 {
		t.Errorf("down from the last row should wrap, got %d", b.sel)
	}
	empty := &browser{}
	empty.move(1)
	if empty.sel != 0 {
		t.Errorf("sel = %d on an empty level", empty.sel)
	}
}

func TestScreenFrames(t *testing.T) {
	b := &browser{rows: browseRows(t.TempDir()), status: "something happened"}
	if raw := b.screen(testNow, true); strings.Contains(strings.ReplaceAll(raw, "\r\n", ""), "\n") {
		t.Errorf("a bare newline survived into the raw frame:\n%q", raw)
	}
	plain := b.screen(testNow, false)
	if strings.Contains(plain, "\r") || !strings.Contains(plain, "something happened") || !strings.Contains(plain, trackingHelp(palette{}, false, false)) {
		t.Errorf("plain frame:\n%s", plain)
	}
}

func TestClipKeepsTheHeaderAndTheCursorVisible(t *testing.T) {
	repos := make([]app.RepoRow, 30)
	for i := range repos {
		repos[i] = app.RepoRow{Name: fmt.Sprintf("repo-%02d", i)}
	}
	b := &browser{
		rows:   []app.AgentRow{{Family: "claude-code", Display: "Claude Code", Repos: repos}},
		open:   "claude-code",
		height: 12,
	}

	lines := strings.Split(strings.TrimSuffix(b.screen(testNow, false), "\n"), "\n")
	if len(lines) > b.height-1 {
		t.Fatalf("frame has %d lines, terminal has %d", len(lines), b.height)
	}
	if !strings.Contains(lines[0], "claude code") {
		t.Errorf("header not pinned: %q", lines[0])
	}
	frame := strings.Join(lines, "\n")
	if !strings.Contains(frame, ">  repo-00") || !strings.Contains(frame, "(24 more below)") {
		t.Errorf("first page:\n%s", frame)
	}

	b.sel = 29
	frame = b.screen(testNow, false)
	if !strings.Contains(frame, ">  repo-29") || !strings.Contains(frame, "(24 more above)") {
		t.Errorf("last page:\n%s", frame)
	}

	b.sel, b.open = 0, ""
	if frame = b.screen(testNow, false); !strings.Contains(frame, ">  Claude Code") {
		t.Errorf("a stale offset survived the level change:\n%s", frame)
	}

	b.height = 0
	b.open, b.sel = "claude-code", 15
	if frame = b.screen(testNow, false); strings.Contains(frame, "more") || !strings.Contains(frame, "repo-29") {
		t.Errorf("without a height every row shows:\n%s", frame)
	}
}

func TestReposSortByDayThenSize(t *testing.T) {
	used := func(daysAgo int, size int64) sources.Candidate {
		return sources.Candidate{MTime: testNow.AddDate(0, 0, -daysAgo), Size: size}
	}
	row := &app.AgentRow{}
	row.Add("/w/old-big", used(3, 500), "", false)
	row.Add("/w/today-small", used(0, 1), "", false)
	row.Add("/w/today-big", used(0, 100), "", false)

	app.SortRepos(row.Repos)
	got := []string{row.Repos[0].Name, row.Repos[1].Name, row.Repos[2].Name}
	want := []string{"today-big", "today-small", "old-big"}
	if !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestAgoAndBytes(t *testing.T) {
	for d, want := range map[time.Duration]string{
		30 * time.Second: "just now", 20 * time.Minute: "20 min ago", 5 * time.Hour: "5 hours ago",
		30 * time.Hour: "1 day ago", 20 * 24 * time.Hour: "20 days ago", 400 * 24 * time.Hour: "13 months ago",
	} {
		if got := app.Ago(testNow.Add(-d), testNow); got != want {
			t.Errorf("%v ago = %q, want %q", d, got, want)
		}
	}
	for in, want := range map[int64]string{0: "0 MB", 1: "1 B", 1<<20 - 1: "1024.0 KB", 1667 << 20: "1667.0 MB"} {
		if got := bytesCell(in); got != want {
			t.Errorf("bytesCell(%d) = %q, want %q", in, got, want)
		}
	}
	if toSync(0, true) != "0 MB" || toSync(0, false) != "n/a" {
		t.Error("nothing to send is 0 MB, unknown is n/a")
	}
}

func TestAgentRowAggregatesPerRepository(t *testing.T) {
	var a app.AgentRow
	a.Add("/w/acme", sources.Candidate{Size: 100, MTime: testNow}, "", false)
	a.Add("/w/acme", sources.Candidate{Size: 200, MTime: testNow.Add(time.Hour)}, "", true)
	a.Add("", sources.Candidate{Size: 50, MTime: testNow.Add(-time.Hour)}, "", true)
	if a.Bytes != 350 || a.Pending != 250 || !a.Last.Equal(testNow.Add(time.Hour)) {
		t.Errorf("totals = %+v", a)
	}
	if len(a.Repos) != 2 || a.Repos[0].Name != "acme" || a.Repos[0].Bytes != 300 || a.Repos[0].Pending != 200 || a.Repos[1].Name != "" {
		t.Errorf("repos = %+v", a.Repos)
	}
}

func renderRows() []app.AgentRow {
	return []app.AgentRow{
		{Family: "claude-code", Display: "Claude Code", Bytes: 1747 << 20, Pending: 40 << 20, PendingKnown: true, Last: testNow.Add(-time.Minute),
			Repos: []app.RepoRow{
				{Name: "keeper", Bytes: 100 << 20, Pending: 40 << 20, Last: testNow.Add(-2 * time.Hour)},
				{Name: "client-acme", Bytes: 48 << 20, Last: testNow.Add(-72 * time.Hour), Off: true},
				{Name: "", Bytes: 1 << 10, Last: testNow.Add(-time.Hour)},
			}},
		{Family: "cursor", Display: "Cursor"},
		{Family: "codex", Display: "Codex"},
	}
}

func TestPlainRendering(t *testing.T) {
	rows := renderRows()
	agents := renderAgents(rows, 0, testNow, palette{})
	repos := renderRepos(rows[0], 1, testNow, palette{})
	for _, want := range []string{"Cursor", "not installed", "1747.0 MB", "40.0 MB"} {
		if !strings.Contains(agents, want) {
			t.Errorf("agents table lacks %q:\n%s", want, agents)
		}
	}
	for _, want := range []string{"client-acme", "not tracked", "(no repository found)", "100.0 MB", "48.0 MB"} {
		if !strings.Contains(repos, want) {
			t.Errorf("repos table lacks %q:\n%s", want, repos)
		}
	}
	lines := strings.Split(strings.TrimRight(repos, "\n"), "\n")
	if strings.Index(lines[1], "100.0 MB")+8 != strings.Index(lines[2], "48.0 MB")+7 {
		t.Errorf("sizes do not share a right edge:\n%s", repos)
	}
	if !strings.Contains(lines[2], "n/a") || strings.Count(strings.Fields(lines[2])[0], ">") != 1 {
		t.Errorf("an excluded row must not claim an empty queue, and one row carries the cursor:\n%s", repos)
	}
	for _, line := range append(lines, strings.Split(agents, "\n")...) {
		if line != strings.TrimRight(line, " ") || strings.Contains(line, "\x1b") {
			t.Errorf("padding or escapes in a plain line: %q", line)
		}
	}
}

var ansiSeq = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// Stripped of escapes, the painted table is the plain one byte for byte.
func TestColourRendering(t *testing.T) {
	pal := ansiPalette()
	rows := renderRows()
	for _, pair := range [][2]string{
		{renderAgents(rows, 0, testNow, pal), renderAgents(rows, 0, testNow, palette{})},
		{renderRepos(rows[0], 0, testNow, pal), renderRepos(rows[0], 0, testNow, palette{})},
		{trackingHelp(pal, true, false), trackingHelp(palette{}, true, false)},
	} {
		if got := ansiSeq.ReplaceAllString(pair[0], ""); got != pair[1] {
			t.Errorf("colour changed the layout:\n%q\nwant\n%q", got, pair[1])
		}
	}
	lines := strings.Split(strings.TrimRight(renderAgents(rows, 1, testNow, pal), "\n"), "\n")
	if !strings.HasPrefix(lines[0], pal.dim) || strings.HasPrefix(lines[1], pal.dim) || !strings.HasPrefix(lines[3], pal.dim) {
		t.Errorf("header dim, collected row plain, absent row dim:\n%q", lines)
	}
	if !strings.HasPrefix(lines[2], pal.dim) || !strings.Contains(lines[2], pal.bold+">") {
		t.Errorf("the selected excluded row keeps its dim and shows the cursor: %q", lines[2])
	}
	if !strings.Contains(lines[1], "    "+pal.cyan+"3"+pal.reset) || !strings.Contains(lines[1], pal.cyan+"1747.0"+pal.reset+" MB") || strings.Contains(lines[3], pal.cyan+" ") {
		t.Errorf("numbers cyan, units plain, blanks unpainted: %q / %q", lines[1], lines[3])
	}
	if !strings.Contains(trackingHelp(pal, true, false), pal.cyan+"t"+pal.reset) {
		t.Errorf("keys are painted: %q", trackingHelp(pal, true, false))
	}
	on, off := trackingHelp(palette{}, true, false), trackingHelp(palette{}, true, true)
	if !strings.Contains(on, "stop tracking") || !strings.Contains(off, "resume tracking") || len(on) != len(off) {
		t.Errorf("the t key names what it will do, at one width: %q / %q", on, off)
	}
	if strings.Contains(trackingHelp(pal, false, false), "tracking") {
		t.Error("the agent level offers no toggle")
	}
}

// --- editing ----------------------------------------------------------------

// browseRows is a survey over dirs, optionally with client-acme under markers.
func browseRows(dir string, markers ...string) []app.AgentRow {
	return []app.AgentRow{
		{Family: "claude-code", Display: "Claude Code", Repos: []app.RepoRow{
			{Name: "keeper", Dir: filepath.Join(dir, "keeper")},
			{Name: "client-acme", Dir: filepath.Join(dir, "client-acme"), Off: len(markers) > 0, Markers: markers},
			{Name: ""},
		}},
		{Family: "cursor", Display: "Cursor"},
	}
}

func TestToggle(t *testing.T) {
	home := t.TempDir()
	for _, name := range []string{"keeper", "client-acme"} {
		if err := os.MkdirAll(filepath.Join(home, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	catalog, err := sources.Load()
	if err != nil {
		t.Fatal(err)
	}
	attr := catalog.RepoFilter()
	toggle := func(rows []app.AgentRow, open string, sel int) (string, string) {
		t.Helper()
		b := &browser{rows: rows, attr: attr, pal: ansiPalette(), open: open, sel: sel}
		if err := b.toggle(); err != nil {
			t.Fatal(err)
		}
		return b.status, b.statusStyle
	}
	pal := ansiPalette()
	acme := filepath.Join(home, "client-acme")
	own := sources.MarkerPath(acme)
	above := sources.MarkerPath(home)
	worktree := sources.MarkerPath(filepath.Join(home, "wt", "stray"))

	if msg, _ := toggle(browseRows(home), "claude-code", 1); msg != "" {
		t.Errorf("a successful toggle should say nothing, got %q", msg)
	}
	if _, ok := attr.Marker(acme); !ok {
		t.Fatal("the marker was not written")
	}
	if msg, _ := toggle(browseRows(home, own), "claude-code", 1); msg != "" {
		t.Errorf("a successful toggle should say nothing, got %q", msg)
	}
	if _, ok := attr.Marker(acme); ok {
		t.Fatal("the marker was not removed")
	}
	if msg, style := toggle(browseRows(home, above), "claude-code", 1); style != pal.dim || !strings.Contains(msg, above) {
		t.Errorf("an ancestor marker should be named: got %q", msg)
	}
	if err := attr.Untrack(acme); err != nil {
		t.Fatal(err)
	}
	if msg, style := toggle(browseRows(home, worktree, own), "claude-code", 1); style != pal.dim || !strings.Contains(msg, worktree) || strings.Contains(msg, own) {
		t.Errorf("the remaining worktree marker should be named: got %q", msg)
	}
	if _, ok := attr.Marker(acme); ok {
		t.Fatal("the repository's own marker was not removed")
	}
	if msg, style := toggle(browseRows(home), "claude-code", 2); style != pal.dim || !strings.Contains(msg, "no repository") {
		t.Errorf("unattributed sessions cannot be marked: got %q", msg)
	}
	// The agent level has nothing to toggle.
	if msg, _ := toggle(browseRows(home), "", 1); msg != "" {
		t.Errorf("agent-level toggle must be a no-op, got %q", msg)
	}
}

func TestEndNoteSaysTheNetChange(t *testing.T) {
	pal := ansiPalette()
	home := t.TempDir()
	keeper, acme := filepath.Join(home, "keeper"), filepath.Join(home, "client-acme")
	b := &browser{pal: pal, rows: browseRows(home, sources.MarkerPath(acme))}
	before := map[string]bool{keeper: false, acme: false}

	note := b.endNote(before)
	if !strings.HasPrefix(note, styled(pal.dim, "tracking changes:", pal.reset)) {
		t.Errorf("end note lacks its heading:\n%s", note)
	}
	want := "  " + styled(pal.bold, "client-acme", pal.reset) + styled(pal.yellow, ": no longer tracked", pal.reset)
	if !strings.Contains(note, want) {
		t.Errorf("end note lacks %q:\n%s", want, note)
	}
	if strings.Contains(note, "keeper") {
		t.Errorf("an unchanged repository has no bullet:\n%s", note)
	}

	if note := b.endNote(b.marked()); note != "" {
		t.Errorf("toggling back and forth is not a change, got %q", note)
	}
	if note := b.endNote(map[string]bool{keeper: true, acme: true}); !strings.Contains(note,
		"tracked again, everything not yet sent goes on the next run") {
		t.Errorf("a lifted marker is reported: %q", note)
	}
}
