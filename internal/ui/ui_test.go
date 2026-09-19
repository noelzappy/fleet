package ui

import (
	"io"
	"strings"
	"testing"

	"github.com/muesli/termenv"
)

func colour() Palette {
	p := NewPalette(io.Discard)
	p.R.SetColorProfile(termenv.ANSI256)
	return p
}

func TestStyleIsANoOpWithoutColour(t *testing.T) {
	p := NewPalette(io.Discard) // not a terminal: Ascii profile
	in := "✓ docker\n● login\n→ curl -fsS http://x\n  detail\nsync: nothing to do\nplain\n"
	if got := Style(p, in); got != in {
		t.Errorf("Style changed uncoloured output:\n%q", got)
	}
}

func TestStyleMarkers(t *testing.T) {
	p := colour()
	cases := map[string]string{
		"✓ docker":                      "✓",
		"● multica login":               "●",
		"→ curl -fsS http://x":          "curl -fsS http://x",
		"✗ boom":                        "boom",
		"sync: harness codex is signed": "codex",
	}
	for in, contains := range cases {
		out := Style(p, in+"\n")
		if !strings.Contains(out, "\x1b[") {
			t.Errorf("%q not styled: %q", in, out)
		}
		if !strings.Contains(out, contains) || !strings.HasSuffix(out, "\n") {
			t.Errorf("%q lost text or newline: %q", in, out)
		}
	}
	if got := Style(p, "no marker here\n"); got != "no marker here\n" {
		t.Errorf("unmarked line changed: %q", got)
	}
	// Multi-line input is styled line by line, blank lines kept.
	out := Style(p, "✓ a\n\n→ b\n")
	if strings.Count(out, "\n") != 3 {
		t.Errorf("line structure changed: %q", out)
	}
}
