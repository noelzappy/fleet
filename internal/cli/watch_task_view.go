package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/noelzappy/fleet/internal/ui"
)

// intent is what Enter does in the task view: ask a model about the task, or tell its
// agent something.
type intent int

const (
	intentAsk intent = iota
	intentTell
)

// taskView is everything the task screen needs. The model owns the text input and hands
// the renderer its already-drawn line, so this file stays free of the input widget.
type taskView struct {
	W, H     int
	Detail   *taskDetail
	Thread   []exchange
	Intent   intent
	Input    string // the input line as drawn by the widget, cursor included
	Confirm  string // non-empty while a follow-up waits for the owner's yes
	Busy     bool   // a question is with the model
	Loading  bool
	Err      error
	Off      int // scroll of the context section, from the top
	ThreadUp int // how far the conversation is scrolled back from its newest line
	Harness  string
	Now      time.Time
}

func relTime(now, t time.Time) string {
	if t.IsZero() {
		return "?"
	}
	d := now.Sub(t).Round(time.Second)
	switch {
	case d < 0:
		return "now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return t.Local().Format("Jan 2")
}

// wrap breaks s into lines of at most w cells, indenting continuation lines.
func wrap(s string, w int, indent string) []string {
	if w < 10 {
		w = 10
	}
	var out []string
	for _, para := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if strings.TrimSpace(para) == "" {
			out = append(out, "")
			continue
		}
		for i, l := range strings.Split(ansi.Wrap(para, w, ""), "\n") {
			if i > 0 {
				l = indent + l
			}
			out = append(out, l)
		}
	}
	return out
}

// contextLines is the scrollable "what is going on" section.
func contextLines(td *taskDetail, p ui.Palette, w int, now time.Time) []string {
	var out []string
	section := func(name string) { out = append(out, p.Dim.Render("· "+name)) }
	if td.Row.Why != "" {
		out = append(out, toneStyle(p, td.Row.Tone).Render("↳ ")+p.Warn.Render(td.Row.Why))
	}
	if !td.HasTask {
		out = append(out, p.Dim.Render("Not dispatched: there is no agent on this issue yet."))
	}
	if len(td.Runs) > 0 {
		section("runs")
		for _, r := range td.Runs {
			sym, st := "●", p.Info
			switch r.Status {
			case "completed":
				sym, st = "✓", p.Good
			case "failed", "cancelled":
				sym, st = "✗", p.Bad
			}
			line := st.Render(sym) + " " + fit(r.Status, 10) + " started " + relTime(now, r.Started)
			if !r.Ended.IsZero() {
				line += p.Dim.Render(fmt.Sprintf("  ran %s", r.Ended.Sub(r.Started).Round(time.Second)))
			}
			out = append(out, line)
			if r.Error != "" {
				for _, l := range wrap(r.Error, w-6, "  ") {
					out = append(out, "  "+p.Bad.Render(l))
				}
			}
		}
	}
	if len(td.Comments) > 0 {
		section("recent comments")
		for _, c := range td.Comments {
			who, st := c.Author, p.Info
			if c.Author == "member" {
				who, st = "you", p.Accent
			}
			out = append(out, st.Render(who)+p.Dim.Render(" "+relTime(now, c.At)))
			lines := wrap(c.Content, w-4, "")
			if len(lines) > 6 {
				lines = append(lines[:6], p.Dim.Render("…"))
			}
			for _, l := range lines {
				out = append(out, "  "+l)
			}
		}
	}
	section("pull request")
	if td.PR == nil {
		out = append(out, p.Dim.Render("none yet"))
	} else {
		line := fmt.Sprintf("#%d %s  ", td.PR.Num, td.PR.Head) + toneStyle(p, td.PR.Tone).Render(orDash(td.PR.Gate))
		if len(td.PR.Flags) > 0 {
			line += "  " + p.Warn.Render(strings.Join(td.PR.Flags, " · "))
		}
		out = append(out, line)
	}
	section("agent's worktree")
	switch wt := td.Work; {
	case wt.Path == "":
		out = append(out, p.Dim.Render("no checkout found for the latest run"))
	default:
		head := "branch " + orDash(wt.Branch)
		if wt.Shortstat != "" {
			head += "  ·  " + wt.Shortstat
		}
		out = append(out, head, p.Dim.Render(wt.Path))
		for _, l := range wt.Log {
			out = append(out, p.Dim.Render("  "+l))
		}
		if len(wt.Changed) == 0 {
			out = append(out, p.Good.Render("clean: nothing uncommitted"))
		} else {
			out = append(out, p.Warn.Render(fmt.Sprintf("%d uncommitted:", len(wt.Changed)+wt.More)))
			for _, l := range wt.Changed {
				out = append(out, "  "+l)
			}
			if wt.More > 0 {
				out = append(out, p.Dim.Render(fmt.Sprintf("  …and %d more", wt.More)))
			}
		}
	}
	for _, n := range td.Notes {
		out = append(out, p.Warn.Render("! "+n))
	}
	return out
}

// threadLines draws the conversation, newest last.
func threadLines(tv *taskView, p ui.Palette, w int) []string {
	var out []string
	for _, e := range tv.Thread {
		var head string
		var body lipgloss.Style
		switch e.Kind {
		case exAsk:
			head, body = p.Accent.Render("you ›"), p.Bold
		case exAnswer:
			head, body = p.Info.Render("fleet ›"), lipgloss.NewStyle()
			if e.Bad {
				head, body = p.Bad.Render("fleet ›"), p.Bad
			}
		case exTell:
			head, body = p.Good.Render("you → "+tv.Detail.Task.Profile+" ✓"), p.Bold
		default:
			head, body = p.Dim.Render("·"), p.Dim
			if e.Bad {
				head, body = p.Bad.Render("!"), p.Bad
			}
		}
		lines := wrap(e.Text, w-lipgloss.Width(head)-1, "")
		for i, l := range lines {
			if i == 0 {
				out = append(out, head+" "+body.Render(l))
			} else {
				out = append(out, strings.Repeat(" ", lipgloss.Width(head)+1)+body.Render(l))
			}
		}
		out = append(out, "")
	}
	if tv.Busy {
		out = append(out, p.Info.Render("fleet ›")+" "+p.Dim.Render("thinking… (asking "+orDash(tv.Harness)+")"))
	}
	return out
}

func renderTask(tv *taskView, p ui.Palette) string {
	td := tv.Detail
	w := tv.W - 2
	var out []string

	title := p.Accent.Render(" fleet") + p.Dim.Render(" ▸ ") + p.Bold.Render(fmt.Sprintf("#%d ", td.Num)) + td.Title
	right := p.Dim.Render("loaded " + tv.Now.Format("15:04:05"))
	if tv.Loading {
		right = p.Info.Render("⟳ ") + right
	}
	gap := max(1, tv.W-lipgloss.Width(title)-lipgloss.Width(right)-1)
	out = append(out, ansi.Truncate(title+strings.Repeat(" ", gap)+right, tv.W, "…"))

	bits := []string{toneStyle(p, td.Row.Tone).Render(orDash(td.Row.State))}
	if td.HasTask {
		bits = append(bits, "agent "+td.Task.Profile)
	}
	if td.PR != nil {
		bits = append(bits, fmt.Sprintf("PR #%d", td.PR.Num))
	}
	if td.URL != "" {
		bits = append(bits, p.Dim.Render(td.URL))
	}
	out = append(out, ansi.Truncate(" "+strings.Join(bits, p.Dim.Render("  ·  ")), tv.W, "…"))

	// fixed: title(2) + two pane titles + input + footer; the rest is split.
	body := max(tv.H-6, 4)
	ctxH := max(3, body*55/100)
	convH := max(2, body-ctxH)
	ctx := contextLines(td, p, w, tv.Now)
	off := clamp(tv.Off, 0, max(0, len(ctx)-ctxH))
	out = append(out, taskBar(p, "Context", off, ctxH, len(ctx), tv.W, true, false))
	for _, l := range ctx[off:min(len(ctx), off+ctxH)] {
		out = append(out, " "+ansi.Truncate(l, tv.W-2, "…"))
	}
	for pad := min(len(ctx), off+ctxH) - off; pad < ctxH; pad++ {
		out = append(out, "")
	}

	conv := threadLines(tv, p, w)
	if len(conv) == 0 {
		for _, l := range wrap(hint(tv), w, "") {
			conv = append(conv, p.Dim.Render(l))
		}
	}
	last := max(0, len(conv)-convH)
	coff := clamp(last-tv.ThreadUp, 0, last)
	out = append(out, taskBar(p, "Conversation", coff, convH, len(conv), tv.W, false, tv.ThreadUp > 0))
	for _, l := range conv[coff:min(len(conv), coff+convH)] {
		out = append(out, " "+ansi.Truncate(l, tv.W-2, "…"))
	}
	for pad := min(len(conv), coff+convH) - coff; pad < convH; pad++ {
		out = append(out, "")
	}

	switch {
	case tv.Confirm != "":
		out = append(out, " "+p.Warn.Render(tv.Confirm))
	default:
		out = append(out, " "+promptLabel(tv, p)+tv.Input)
	}
	if tv.Err != nil {
		out = append(out, p.Bad.Render(" ✗ "+oneLine(tv.Err.Error())))
	} else {
		out = append(out, taskFooter(tv, p))
	}
	return strings.Join(out, "\n")
}

func hint(tv *taskView) string {
	td := tv.Detail
	ask := fmt.Sprintf("Asking sends the context above to %s and can't change anything", tv.Harness)
	if tv.Harness == "" {
		ask = "No signed-in claude-code or codex harness is available to answer questions right now"
	}
	if ok, why := canTell(td); !ok {
		return "Ask anything about this task (" + why + "). " + ask + "."
	}
	return fmt.Sprintf("Ask about this task, or press tab to tell %s something. %s; telling comments on the task and wakes the agent.", td.Task.Profile, ask)
}

func promptLabel(tv *taskView, p ui.Palette) string {
	if tv.Intent == intentTell {
		return p.Good.Render("tell " + tv.Detail.Task.Profile + " ▸ ")
	}
	return p.Accent.Render("ask ▸ ")
}

func taskBar(p ui.Palette, name string, off, h, total, w int, focus, back bool) string {
	st := p.Dim
	if focus {
		st = p.Accent
	}
	scroll := ""
	if total > h {
		scroll = fmt.Sprintf("  %d–%d of %d", off+1, min(total, off+h), total)
		if back {
			scroll += " · end to follow"
		}
	}
	bar := st.Render("── "+name) + p.Dim.Render(scroll+" ")
	return bar + p.Dim.Render(strings.Repeat("─", max(0, w-lipgloss.Width(bar))))
}

func taskFooter(tv *taskView, p ui.Palette) string {
	keys := []string{"esc back", "tab ask/tell", "enter send", "pgup/pgdn scroll", "ctrl+r reload"}
	if tv.Confirm != "" {
		keys = []string{"enter send", "esc cancel"}
	}
	for n := len(keys); n > 0; n-- {
		if s := " " + strings.Join(keys[:n], "  ·  "); lipgloss.Width(s) <= tv.W {
			return p.Dim.Render(s)
		}
	}
	return ""
}
