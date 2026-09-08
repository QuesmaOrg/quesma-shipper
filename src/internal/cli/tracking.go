package cli

import (
	"cmp"
	"fmt"
	"io"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/QuesmaOrg/quesma-shipper/app"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
)

func trackingCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tracking",
		Short: "What is collected, per agent and repository",
		Long: "What is collected, per agent and repository: size, what is still to be sent, when\n" +
			"it was last used. Press `t` on a repository to stop collecting from it (a `.notrajectories`\n" +
			"file is placed there, what was already sent stays sent) or to start again.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return browseTracking(cmd) },
	}
	return cmd
}

func table(heads []string, right []bool, nameCol int, rows [][]string, rowStyle []string, pal palette) string {
	width := make([]int, len(heads))
	measure := func(cells []string) {
		for i, cell := range cells {
			width[i] = max(width[i], utf8.RuneCountInString(cell))
		}
	}
	measure(heads)
	for _, row := range rows {
		measure(row)
	}
	line := func(cells []string, style string, paint bool) string {
		var b strings.Builder
		b.WriteString(style)
		for i, cell := range cells {
			if i > 0 {
				b.WriteString("  ")
			}
			pad := strings.Repeat(" ", width[i]-utf8.RuneCountInString(cell))
			text := cell + pad
			if right[i] {
				text = pad + cell
			}
			switch {
			case !paint || strings.TrimSpace(cell) == "":
			case i == 0 || i == nameCol:
				text = styled(pal.bold, text, pal.reset+style)
			default:
				text = paintValue(text, pal, style)
			}
			b.WriteString(text)
		}
		out := strings.TrimRight(b.String(), " ")
		if style != "" {
			return strings.TrimSuffix(out, style) + pal.reset
		}
		return out
	}
	var b strings.Builder
	b.WriteString(line(heads, pal.dim, false) + "\n")
	for i, row := range rows {
		b.WriteString(line(row, rowStyle[i], true) + "\n")
	}
	return b.String()
}

var leadingNumber = regexp.MustCompile(`^(\s*)(\d[\d,.]*)`)

func paintValue(cell string, pal palette, restore string) string {
	if strings.TrimSpace(cell) == "n/a" {
		return styled(pal.dim, cell, pal.reset+restore)
	}
	return leadingNumber.ReplaceAllString(cell, "$1"+pal.cyan+"$2"+pal.reset+restore)
}
func rowStyle(pal palette, selected, off bool) string {
	switch {
	case off:
		return pal.dim
	case selected:
		return pal.bold
	}
	return ""
}

func renderAgents(rows []app.AgentRow, sel int, now time.Time, pal palette) string {
	cells := make([][]string, len(rows))
	styles := make([]string, len(rows))
	for i, a := range rows {
		state, repos, size, pending, last := "", "", "", "", ""
		if len(a.Repos) == 0 {
			state = "not installed"
		} else {
			repos, size, last = strconv.Itoa(len(a.Repos)), bytesCell(a.Bytes), app.Ago(a.Last, now)
			pending = toSync(a.Pending, a.PendingKnown)
		}
		cells[i] = []string{cursor(i == sel), a.Display, repos, size, pending, last, state}
		styles[i] = rowStyle(pal, i == sel, len(a.Repos) == 0)
	}
	return table([]string{" ", "agent", "repos", "size", "to send", "last used", ""},
		[]bool{false, false, true, true, true, false, false}, 1, cells, styles, pal)
}

func renderRepos(a app.AgentRow, sel int, now time.Time, pal palette) string {
	cells := make([][]string, len(a.Repos))
	styles := make([]string, len(a.Repos))
	for i, r := range a.Repos {
		state, pending := "", "n/a"
		if r.Off {
			state = "not tracked"
		} else {
			pending = toSync(r.Pending, a.PendingKnown)
		}
		cells[i] = []string{cursor(i == sel), cmp.Or(r.Name, "(no repository found)"),
			bytesCell(r.Bytes), pending, app.Ago(r.Last, now), state}
		styles[i] = rowStyle(pal, i == sel, r.Off)
	}
	return table([]string{" ", strings.ToLower(a.Display), "size", "to send", "last used", ""},
		[]bool{false, false, true, true, false, false}, 1, cells, styles, pal)
}

func cursor(on bool) string {
	if on {
		return ">"
	}
	return " "
}
func toSync(n int64, known bool) string {
	if !known {
		return "n/a"
	}
	return bytesCell(n)
}
func bytesCell(n int64) string {
	if n == 0 {
		return "0 MB"
	}
	return app.HumanBytes(n)
}

type key int

const (
	keyNone key = iota
	keyUp
	keyDown
	keyOpen
	keyBack
	keyToggle
	keyQuit
)

func decodeKey(b []byte) key {
	if len(b) >= 3 && b[0] == 0x1b && b[1] == '[' {
		switch b[2] {
		case 'A':
			return keyUp
		case 'B':
			return keyDown
		case 'C':
			return keyOpen
		case 'D':
			return keyBack
		}
		return keyNone
	}
	if len(b) == 0 {
		return keyNone
	}
	switch b[0] {
	case 'k':
		return keyUp
	case 'j':
		return keyDown
	case '\r', '\n', 'l':
		return keyOpen
	case 'h', 'b':
		return keyBack
	case 't', ' ':
		return keyToggle
	case 'q', 0x03, 0x04, 0x1b: // q, Ctrl-C, Ctrl-D, Esc
		return keyQuit
	}
	return keyNone
}

func trackingHelp(pal palette, repoLevel, off bool) string {
	toggle := "stop tracking  "
	if off {
		toggle = "resume tracking"
	}
	keys := [][2]string{{"↑↓", "move"}, {"enter", "open"}, {"t", toggle}, {"b", "back"}, {"q", "quit"}}
	if !repoLevel {
		keys = [][2]string{{"↑↓", "move"}, {"enter", "open"}, {"q", "quit"}}
	}
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, styled(pal.cyan, k[0], pal.reset)+styled(pal.dim, " "+k[1], pal.reset))
	}
	return strings.Join(parts, "  ")
}

func styled(style, s, reset string) string {
	if style == "" {
		return s
	}
	return style + s + reset
}
func browseTracking(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	catalog, err := sources.Load()
	if err != nil {
		return err
	}
	view := &browser{attr: catalog.RepoFilter(), names: app.FamilyNames(catalog), pal: paletteFor(out)}
	if err := view.refresh(); err != nil {
		return err
	}
	stdin, isFile := cmd.InOrStdin().(*os.File)
	if !isFile || !isTerminal(stdin) || !isTerminal(out) {
		fmt.Fprint(out, view.screen(time.Now(), false))
		return nil
	}
	before := view.marked()
	if err := view.interact(stdin, out); err != nil {
		return err
	}
	fmt.Fprint(out, view.endNote(before))
	return nil
}

type browser struct {
	attr  *sources.RepoFilter
	names map[string]string
	rows  []app.AgentRow
	pal   palette

	open        string
	sel         int
	top         int
	height      int
	status      string
	statusStyle string
}

func (b *browser) refresh() error {
	was := ""
	if names := b.level(); b.sel < len(names) {
		was = names[b.sel]
	}
	eff, paths, err := app.ResolveEffective()
	if err != nil {
		return err
	}
	b.rows = app.Survey(eff, paths, b.attr, b.names)
	if b.agent() == nil {
		b.open = ""
	}
	b.sel = max(slices.Index(b.level(), was), 0)
	return nil
}
func (b *browser) level() []string {
	if a := b.agent(); a != nil {
		out := make([]string, len(a.Repos))
		for i, r := range a.Repos {
			out[i] = r.Dir
		}
		return out
	}
	out := make([]string, len(b.rows))
	for i, a := range b.rows {
		out[i] = a.Family
	}
	return out
}
func (b *browser) agent() *app.AgentRow {
	if i := slices.IndexFunc(b.rows, func(a app.AgentRow) bool { return a.Family == b.open }); b.open != "" && i >= 0 {
		return &b.rows[i]
	}
	return nil
}
func (b *browser) clip(tbl string) string {
	lines := strings.Split(strings.TrimSuffix(tbl, "\n"), "\n")
	rows := lines[1:]
	avail := b.height - 4
	if b.status != "" {
		avail -= 2
	}
	if b.height == 0 || len(rows) <= avail {
		return tbl
	}
	fit := max(avail-2, 1)
	b.top = max(min(b.top, b.sel), b.sel-fit+1)
	b.top = max(min(b.top, len(rows)-fit), 0)

	out := lines[:1:1]
	if b.top > 0 {
		out = append(out, styled(b.pal.dim, fmt.Sprintf("   (%d more above)", b.top), b.pal.reset))
	}
	out = append(out, rows[b.top:b.top+fit]...)
	if below := len(rows) - b.top - fit; below > 0 {
		out = append(out, styled(b.pal.dim, fmt.Sprintf("   (%d more below)", below), b.pal.reset))
	}
	return strings.Join(out, "\n") + "\n"
}
func (b *browser) screen(now time.Time, raw bool) string {
	var body string
	if a := b.agent(); a != nil {
		body = renderRepos(*a, b.sel, now, b.pal)
	} else {
		body = renderAgents(b.rows, b.sel, now, b.pal)
	}
	body = b.clip(body)
	if b.status != "" {
		body += "\n" + styled(b.statusStyle, b.status, b.pal.reset) + "\n"
	}
	off := false
	if a := b.agent(); a != nil && b.sel < len(a.Repos) {
		off = a.Repos[b.sel].Off
	}
	body += "\n" + trackingHelp(b.pal, b.agent() != nil, off) + "\n"
	if raw {
		body = strings.ReplaceAll(body, "\n", "\r\n")
	}
	return body
}

func (b *browser) interact(stdin *os.File, out io.Writer) error {
	restore, err := term.MakeRaw(int(stdin.Fd()))
	if err != nil {
		fmt.Fprint(out, b.screen(time.Now(), false))
		return nil
	}
	fmt.Fprint(out, "\x1b[?1049h\x1b[?25l")
	defer func() {
		fmt.Fprint(out, "\x1b[?25h\x1b[?1049l")
		_ = term.Restore(int(stdin.Fd()), restore)
	}()

	buf := make([]byte, 8)
	for {
		if _, h, err := term.GetSize(int(stdin.Fd())); err == nil {
			b.height = h
		}
		fmt.Fprint(out, "\x1b[H\x1b[2J"+b.screen(time.Now(), true))
		n, err := stdin.Read(buf)
		if err != nil || n == 0 {
			return nil
		}
		switch decodeKey(buf[:n]) {
		case keyQuit:
			return nil
		case keyUp:
			b.move(-1)
		case keyDown:
			b.move(1)
		case keyOpen:
			if b.agent() == nil && b.sel < len(b.rows) {
				b.open, b.sel, b.status = b.rows[b.sel].Family, 0, ""
			}
		case keyBack:
			if was := b.open; was != "" {
				b.open, b.status = "", ""
				b.sel = max(slices.Index(b.level(), was), 0)
			}
		case keyToggle:
			if err := b.toggle(); err != nil {
				return err
			}
			if err := b.refresh(); err != nil {
				return err
			}
		}
	}
}

func (b *browser) move(by int) {
	if n := len(b.level()); n > 0 {
		b.sel = (b.sel + by + n) % n
	}
}
func (b *browser) toggle() error {
	a := b.agent()
	if a == nil || b.sel >= len(a.Repos) {
		return nil
	}
	r := a.Repos[b.sel]
	if r.Dir == "" {
		b.status, b.statusStyle = "these sessions belong to no repository, so tracking cannot be switched off for them", b.pal.dim
		return nil
	}
	if !r.Off {
		b.status = ""
		return b.attr.Untrack(r.Dir)
	}
	own := sources.MarkerPath(r.Dir)
	if !slices.Contains(r.Markers, own) {
		b.status, b.statusStyle = "not tracked because of "+strings.Join(r.Markers, ", ")+", remove that file to track everything under it", b.pal.dim
		return nil
	}
	b.status = ""
	if others := slices.DeleteFunc(slices.Clone(r.Markers), func(m string) bool { return m == own }); len(others) > 0 {
		b.status, b.statusStyle = "some sessions stay untracked because of "+strings.Join(others, ", "), b.pal.dim
	}
	return b.attr.Track(r.Dir)
}
func (b *browser) endNote(before map[string]bool) string {
	after := b.marked()
	var lines []string
	for _, dir := range slices.Sorted(maps.Keys(after)) {
		was, now, name := before[dir], after[dir], sources.RepoName(dir)
		switch {
		case now && !was:
			lines = append(lines, "  "+styled(b.pal.bold, name, b.pal.reset)+
				styled(b.pal.yellow, ": no longer tracked", b.pal.reset))
		case was && !now:
			lines = append(lines, "  "+styled(b.pal.bold, name, b.pal.reset)+
				styled(b.pal.green, ": tracked again, everything not yet sent goes on the next run", b.pal.reset))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return styled(b.pal.dim, "tracking changes:", b.pal.reset) + "\n" + strings.Join(lines, "\n") + "\n"
}
func (b *browser) marked() map[string]bool {
	out := map[string]bool{}
	for _, a := range b.rows {
		for _, r := range a.Repos {
			if r.Dir != "" {
				out[r.Dir] = r.Off
			}
		}
	}
	return out
}
