// Package ui styles fleet's terminal output. Everything is plain text unless the stream is
// a terminal that supports colour: NO_COLOR, pipes, files and `TERM=dumb` all get the same
// bytes fleet has always printed, so logs, launchd/systemd output and CI stay greppable.
//
// It must not import other fleet packages (shell imports it).
package ui

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// Palette is the set of styles for one output stream. Colours are ANSI 256 so they read on
// both light and dark terminals; the semantic names are what the rest of fleet uses.
type Palette struct {
	R                                        *lipgloss.Renderer
	Good, Bad, Warn, Info, Dim, Bold, Accent lipgloss.Style
}

// NewPalette builds a palette that decides colour from w (a terminal, or not).
func NewPalette(w io.Writer) Palette {
	r := lipgloss.NewRenderer(w)
	fg := func(c string) lipgloss.Style { return r.NewStyle().Foreground(lipgloss.Color(c)) }
	return Palette{
		R:      r,
		Good:   fg("42"),
		Bad:    fg("203").Bold(true),
		Warn:   fg("214"),
		Info:   fg("45"),
		Dim:    fg("244"),
		Bold:   r.NewStyle().Bold(true),
		Accent: fg("141").Bold(true),
	}
}

// Err styles stderr, where fleet prints its progress; Out styles stdout (status, tables).
var (
	Err = NewPalette(os.Stderr)
	Out = NewPalette(os.Stdout)
)

// Errf and Errln print to stderr, styled by what the line says (see Style). Calls read like
// fmt.Fprintf(os.Stderr, …), which they replace.
func Errf(format string, a ...any) { fmt.Fprint(os.Stderr, Style(Err, fmt.Sprintf(format, a...))) }
func Errln(a ...any)               { fmt.Fprint(os.Stderr, Style(Err, fmt.Sprintln(a...))) }

// Fail prints a fatal error the way every command ends.
func Fail(err error) { Errf("✗ %v\n", err) }

// Style colours each line of s by its leading marker, the convention fleet's messages
// already follow: ✓ done, ● doing, → a command being run, two spaces for detail, "sync:"
// for the reconciler, ✗ for failures. Lines with no marker pass through unchanged, and so
// does everything when the stream has no colour.
func Style(p Palette, s string) string {
	if p.R.ColorProfile() == termenv.Ascii { // no colour: keep the exact bytes
		return s
	}
	lines := strings.SplitAfter(s, "\n")
	for i, l := range lines {
		lines[i] = styleLine(p, l)
	}
	return strings.Join(lines, "")
}

func styleLine(p Palette, l string) string {
	body := strings.TrimRight(l, "\n")
	nl := l[len(body):]
	switch {
	case body == "":
		return l
	case strings.HasPrefix(body, "✓ "):
		return p.Good.Render("✓") + " " + body[len("✓ "):] + nl
	case strings.HasPrefix(body, "● "):
		return p.Info.Render("●") + " " + p.Bold.Render(body[len("● "):]) + nl
	case strings.HasPrefix(body, "→ "), strings.HasPrefix(body, "  "):
		return p.Dim.Render(body) + nl
	case strings.HasPrefix(body, "Error:"): // older log lines, from before errors printed as ✗
		return p.Bad.Render(body) + nl
	case strings.HasPrefix(body, "✗ "):
		return p.Bad.Render("✗") + " " + p.Bad.Render(body[len("✗ "):]) + nl
	case strings.HasPrefix(body, "warning:"), strings.HasPrefix(body, "sync: harness"), strings.HasPrefix(body, "sync: can't"):
		return p.Warn.Render(body) + nl
	case strings.HasPrefix(body, "sync: "):
		return p.Dim.Render(body) + nl
	}
	return l
}

// IsTerminal reports whether f is an interactive terminal.
func IsTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// Live rewrites one status line in place on a terminal (a poll counter, a wait) and does
// nothing when stderr is a pipe or a log, where a stream of \r updates would be noise.
// LiveDone clears it before the next real line.
func Live(format string, a ...any) {
	if IsTerminal(os.Stderr) {
		fmt.Fprint(os.Stderr, "\r\x1b[2K", Err.Dim.Render(fmt.Sprintf(format, a...)))
	}
}

func LiveDone() {
	if IsTerminal(os.Stderr) {
		fmt.Fprint(os.Stderr, "\r\x1b[2K")
	}
}
