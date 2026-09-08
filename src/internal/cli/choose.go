package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var (
	errNoTerminal = errors.New("no terminal to ask on")
	errCancelled  = errors.New("cancelled")
)

func choose(cmd *cobra.Command, items []string) (int, error) {
	stdin, ok := cmd.InOrStdin().(*os.File)
	out := cmd.OutOrStdout()
	if !ok || !isTerminal(stdin) || !isTerminal(out) {
		return 0, errNoTerminal
	}
	restore, err := term.MakeRaw(int(stdin.Fd()))
	if err != nil {
		return 0, errNoTerminal
	}
	defer func() { _ = term.Restore(int(stdin.Fd()), restore) }()
	pal := paletteFor(out)
	sel := 0
	draw := func() {
		var b strings.Builder
		for i, it := range items {
			if i == sel {
				b.WriteString(styled(pal.bold, "> "+it, pal.reset))
			} else {
				b.WriteString("  " + it)
			}
			b.WriteString("\r\n")
		}
		b.WriteString("\r\n" + styled(pal.cyan, "↑↓", pal.reset) + styled(pal.dim, " move", pal.reset) + "  " +
			styled(pal.cyan, "enter", pal.reset) + styled(pal.dim, " choose", pal.reset) + "  " +
			styled(pal.cyan, "q", pal.reset) + styled(pal.dim, " cancel", pal.reset) + "\r\n")
		fmt.Fprint(out, b.String())
	}
	height := len(items) + 2
	fmt.Fprint(out, "\x1b[?25l")
	defer fmt.Fprint(out, "\x1b[?25h")
	draw()
	buf := make([]byte, 8)
	for {
		n, err := stdin.Read(buf)
		if err != nil || n == 0 {
			return 0, errNoTerminal
		}
		switch decodeKey(buf[:n]) {
		case keyUp:
			sel = (sel + len(items) - 1) % len(items)
		case keyDown:
			sel = (sel + 1) % len(items)
		case keyOpen, keyToggle:
			fmt.Fprintf(out, "\x1b[%dA", height)
			clearLines(out, height)
			return sel, nil
		case keyQuit:
			fmt.Fprintf(out, "\x1b[%dA", height)
			clearLines(out, height)
			return 0, errCancelled
		default:
			continue
		}
		fmt.Fprintf(out, "\x1b[%dA", height)
		draw()
	}
}

func clearLines(out io.Writer, n int) {
	for i := 0; i < n; i++ {
		fmt.Fprint(out, "\x1b[2K\r\n")
	}
	fmt.Fprintf(out, "\x1b[%dA", n)
}

func confirm(cmd *cobra.Command, question string) (agreed bool, err error) {
	stdin, ok := cmd.InOrStdin().(*os.File)
	if !ok || !isTerminal(stdin) || !isTerminal(cmd.OutOrStdout()) {
		return false, errNoTerminal
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s y/N ", question)
	var line string
	fmt.Fscanln(stdin, &line)
	l := strings.ToLower(strings.TrimSpace(line))
	return l == "y" || l == "yes", nil
}
