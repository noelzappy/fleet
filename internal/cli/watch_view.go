package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/noelzappy/fleet/internal/ui"
)

type pane int

const (
	paneIssues pane = iota
	panePRs
	panePlan
	paneLog
	numPanes
)

var paneNames = [numPanes]string{"Issues", "Pull requests", "Next sync tick", "Activity"}

// snapshotLogLines is how much of the activity log `watch --once` prints.
const snapshotLogLines = 15

// paneWeights split the height left after the header and footer.
var paneWeights = [numPanes]int{4, 2, 2, 3}

// viewState is what the renderer needs besides the snapshot: it holds no terminal handle,
// so tests render a dashboard into a string.
type viewState struct {
	W, H  int
	Focus pane
	Off   [numPanes]int
	// LogBack is how far the activity log is scrolled up from its newest line. 0 follows
	// the tail, which is what a log is for; the other panes scroll from the top (Off).
	LogBack  int
	Zoom     bool
	Verbose  bool // show the → command traces in the activity log
	LogSel   int  // 0 = sync log, 1 = daemon log
	Loading  bool
	Err      error
	Interval time.Duration
	Now      time.Time
}

func toneStyle(p ui.Palette, t tone) lipgloss.Style {
	switch t {
	case toneBad:
		return p.Bad
	case toneWarn:
		return p.Warn
	case toneInfo:
		return p.Info
	case toneGood:
		return p.Good
	case toneAccent:
		return p.Accent
	}
	return p.Dim
}

// fit truncates s to w cells (with an ellipsis) and pads it to exactly w.
func fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	s = ansi.Truncate(s, w, "…")
	if pad := w - lipgloss.Width(s); pad > 0 {
		s += strings.Repeat(" ", pad)
	}
	return s
}

// paneLines returns a pane's lines and how many items they represent (a placeholder line
// like "no open pull requests" is zero items).
func (v viewState) paneLines(s *snapshot, p ui.Palette, which pane) ([]string, int) {
	w := v.W - 2 // one column of margin each side
	switch which {
	case paneIssues:
		return issueLines(s, p, w), len(s.Issues)
	case panePRs:
		return prLines(s, p, w), len(s.PRs)
	case panePlan:
		return planLines(s, p, w), len(s.Plan)
	default:
		lines := v.logPaneLines(s, p, w)
		return lines, len(lines)
	}
}

func issueLines(s *snapshot, p ui.Palette, w int) []string {
	if len(s.Issues) == 0 {
		return []string{p.Dim.Render("no open issues")}
	}
	const numW, stateW, agentW, prW, gateW = 5, 16, 16, 6, 14
	titleW := w - (numW + stateW + agentW + prW + gateW + 5)
	if titleW < 12 {
		titleW = 12
	}
	head := p.Dim.Render(fit("#", numW) + " " + fit("STATE", stateW) + " " + fit("ISSUE", titleW) + " " + fit("AGENT", agentW) + " " + fit("PR", prW) + " " + fit("GATE", gateW))
	lines := []string{head}
	for _, r := range s.Issues {
		pr := ""
		if r.PR != 0 {
			pr = fmt.Sprintf("#%d", r.PR)
		}
		lines = append(lines, fit(fmt.Sprintf("#%d", r.Num), numW)+" "+
			toneStyle(p, r.Tone).Render(fit(r.State, stateW))+" "+
			fit(r.Title, titleW)+" "+
			p.Dim.Render(fit(r.Agent, agentW))+" "+fit(pr, prW)+" "+
			toneStyle(p, gateTone(r.Gate)).Render(fit(r.Gate, gateW)))
		if r.Why != "" {
			lines = append(lines, strings.Repeat(" ", numW+1)+p.Warn.Render("↳ "+r.Why))
		}
	}
	return lines
}

func gateTone(g string) tone {
	switch {
	case strings.HasPrefix(g, "✓"):
		return toneGood
	case strings.HasPrefix(g, "✗"):
		return toneBad
	case strings.HasPrefix(g, "…"):
		return toneInfo
	}
	return toneDim
}

func prLines(s *snapshot, p ui.Palette, w int) []string {
	if len(s.PRs) == 0 {
		return []string{p.Dim.Render("no open pull requests")}
	}
	var lines []string
	for _, r := range s.PRs {
		issue := ""
		if r.Issue != 0 {
			issue = fmt.Sprintf("for #%d", r.Issue)
		}
		line := fit(fmt.Sprintf("#%d", r.Num), 5) + " " + p.Dim.Render(fit(issue, 9)) + " " + toneStyle(p, r.Tone).Render(fit(r.Gate, 14)) + " " + p.Dim.Render(r.Head)
		if len(r.Flags) > 0 {
			line += "  " + p.Warn.Render(strings.Join(r.Flags, " · "))
		}
		lines = append(lines, line)
	}
	return lines
}

func planLines(s *snapshot, p ui.Palette, w int) []string {
	var lines []string
	for _, n := range s.State.Notes {
		lines = append(lines, p.Warn.Render(strings.TrimPrefix(n, "sync: ")))
	}
	if len(s.Plan) == 0 {
		return append(lines, p.Dim.Render("nothing to do: the next tick will change nothing"))
	}
	for _, a := range s.Plan {
		t := toneInfo
		switch a.Kind {
		case "stuck", "escalate", "escalate-failure":
			t = toneWarn
		case "create-task", "create-review", "unblock", "retry", "rerun":
			t = toneAccent
		}
		lines = append(lines, toneStyle(p, t).Render("▸ ")+a.String())
	}
	return lines
}

func (v viewState) logPaneLines(s *snapshot, p ui.Palette, w int) []string {
	src := s.Logs[v.LogSel]
	var lines []string
	for _, l := range src {
		if !v.Verbose && strings.HasPrefix(l, "→ ") {
			continue // the command trace: useful when debugging, noise when watching
		}
		lines = append(lines, ui.Style(p, l))
	}
	if len(lines) == 0 {
		return []string{p.Dim.Render("no log output yet")}
	}
	return lines
}

// heights gives each visible pane its content height. Each pane starts with a share of
// what is left after the header and footer (by paneWeights, at least 2 lines); a pane that
// needs less hands its unused lines to the panes that overflow, so an empty PR list doesn't
// keep four blank lines while the issue table is cut off. lens is each pane's line count.
// Zoom gives everything to the focused pane.
func (v viewState) heights(headerFooter int, lens [numPanes]int) (h [numPanes]int, visible [numPanes]bool) {
	avail := v.H - headerFooter
	if v.Zoom {
		visible[v.Focus], h[v.Focus] = true, max(avail-1, 1)
		return
	}
	total := 0
	for _, w := range paneWeights {
		total += w
	}
	body := avail - int(numPanes) // one title line per pane
	spare, shared := 0, 0
	for i := range paneWeights {
		visible[i] = true
		share := max(2, body*paneWeights[i]/total)
		shared += share
		if need := max(1, lens[i]); need < share {
			h[i], spare = need, spare+share-need
		} else {
			h[i] = share
		}
	}
	spare += max(0, body-shared) // lines lost to integer division are spare too
	for spare > 0 {
		moved := false
		for i := range paneWeights { // heaviest panes first: they are listed first
			if spare > 0 && lens[i] > h[i] {
				h[i]++
				spare--
				moved = true
			}
		}
		if !moved {
			break
		}
	}
	return
}

// render draws the whole dashboard. When the snapshot is nil (first load) it shows a
// waiting line; otherwise the panes are clipped to their heights and the focused one
// scrolls by v.Off.
func render(s *snapshot, v viewState, p ui.Palette, unclipped bool) string {
	var out []string
	out = append(out, header(s, v, p)...)
	if s == nil {
		out = append(out, "", p.Dim.Render("  collecting…"))
		return strings.Join(out, "\n")
	}
	headerFooter := len(out) + 2 // footer + error line
	var all [numPanes][]string
	var counts, lens [numPanes]int
	for i := pane(0); i < numPanes; i++ {
		all[i], counts[i] = v.paneLines(s, p, i)
		lens[i] = len(all[i])
	}
	heights, visible := v.heights(headerFooter, lens)
	for i := pane(0); i < numPanes; i++ {
		if !visible[i] {
			continue
		}
		lines, count := all[i], counts[i]
		h := heights[i]
		if unclipped {
			if i == paneLog && len(lines) > snapshotLogLines { // a snapshot wants the recent past, not the whole file
				lines = lines[len(lines)-snapshotLogLines:]
			}
			h = len(lines)
		}
		out = append(out, paneTitle(p, i, count, len(lines), v, h, v.offset(i, len(lines), h)))
		off := v.offset(i, len(lines), h)
		for _, l := range lines[off:min(len(lines), off+h)] {
			out = append(out, " "+ansi.Truncate(l, v.W-2, "…"))
		}
		if !unclipped {
			for pad := min(len(lines), off+h) - off; pad < h; pad++ {
				out = append(out, "")
			}
		}
	}
	if v.Err != nil {
		out = append(out, p.Bad.Render(" ✗ refresh failed, showing the last good data: "+oneLine(v.Err.Error())))
	}
	return strings.Join(out, "\n")
}

func header(s *snapshot, v viewState, p ui.Palette) []string {
	title := p.Accent.Render(" fleet") + p.Dim.Render(" ▸ ")
	if s == nil {
		return []string{title + "starting…"}
	}
	svc := func(name, state string) string {
		st := p.Bad
		switch state {
		case "active":
			st = p.Good
		case "loaded":
			st = p.Info
		}
		return p.Dim.Render(name+" ") + st.Render("● "+orDash(state))
	}
	paused := p.Dim.Render("paused ") + p.Dim.Render("no")
	if s.Paused {
		paused = p.Dim.Render("paused ") + p.Warn.Render("yes")
	}
	left := title + p.Bold.Render(s.Project) + p.Dim.Render(" ("+s.Repo+")") + "   " + svc("daemon", s.Daemon) + "   " + svc("sync", s.Timer) + "   " + paused
	right := p.Dim.Render(s.At.Format("15:04:05"))
	switch {
	case v.Loading:
		right = p.Info.Render("⟳ refreshing ") + right
	case s.Took > 0:
		right += p.Dim.Render(fmt.Sprintf(" · %.1fs", s.Took.Seconds()))
	}
	gap := max(1, v.W-lipgloss.Width(left)-lipgloss.Width(right)-1)
	line1 := left + strings.Repeat(" ", gap) + right

	var chips []string
	for _, h := range s.Harness {
		chips = append(chips, toneStyle(p, h.Tone).Render("● ")+h.Name+p.Dim.Render(" "+h.Status))
	}
	line2 := " " + p.Dim.Render("harnesses ") + strings.Join(chips, p.Dim.Render("   "))
	return []string{ansi.Truncate(line1, v.W, "…"), ansi.Truncate(line2, v.W, "…")}
}

// offset is the first visible line of a pane: from the top for lists, from the bottom for
// the log, clamped to what exists.
func (v viewState) offset(which pane, total, h int) int {
	last := max(0, total-h)
	if which == paneLog {
		return clamp(last-v.LogBack, 0, last)
	}
	return clamp(v.Off[which], 0, last)
}

func paneTitle(p ui.Palette, which pane, count, total int, v viewState, h, off int) string {
	name := fmt.Sprintf("%s (%d)", paneNames[which], count)
	if which == paneLog {
		src := "sync"
		if v.LogSel == 1 {
			src = "daemon"
		}
		name = fmt.Sprintf("Activity · %s log", src)
	}
	st := p.Dim
	if v.Focus == which {
		st = p.Accent
	}
	scroll := ""
	if total > h {
		scroll = fmt.Sprintf("  %d–%d of %d", off+1, min(total, off+h), total)
		if which == paneLog && v.LogBack > 0 {
			scroll += " · G to follow"
		}
	}
	bar := st.Render("── "+name) + p.Dim.Render(scroll+" ")
	fill := max(0, v.W-lipgloss.Width(bar))
	return bar + p.Dim.Render(strings.Repeat("─", fill))
}

// footer lists the keys that fit: the full set when there is room, then fewer, so it is
// always one line (a wrapped footer pushes the panes off the screen).
func footer(p ui.Palette, v viewState) string {
	sets := [][]string{
		{"q quit", "r refresh", "tab pane", "↑↓ scroll", "z zoom", "l log", "v traces", "? help", fmt.Sprintf("every %s", v.Interval)},
		{"q quit", "r refresh", "tab pane", "↑↓ scroll", "z zoom", "? help"},
		{"q quit", "tab pane", "? help"},
	}
	for _, keys := range sets {
		if s := " " + strings.Join(keys, "  ·  "); lipgloss.Width(s) <= v.W {
			return p.Dim.Render(s)
		}
	}
	return p.Dim.Render(ansi.Truncate(" q quit · ? help", max(v.W, 1), ""))
}

func helpText(p ui.Palette) string {
	return strings.Join([]string{
		"",
		p.Accent.Render("  fleet watch") + p.Dim.Render(" — read-only: it never changes GitHub, Multica or the fleet's state"),
		"",
		"  tab / shift-tab   next / previous pane        ↑ ↓ / j k    scroll the focused pane",
		"  g / G             top / bottom                 pgup / pgdn  scroll a page",
		"  z                 zoom the focused pane        l            switch the log: sync ⇄ daemon",
		"  v                 show / hide command traces   r            refresh now",
		"  ?                 close this help              q / ctrl-c   quit",
		"",
		p.Dim.Render("  Issues: what needs you sorts first (stuck, needs-*, failed), then active work, then the queue."),
		p.Dim.Render("  Next sync tick: exactly what `fleet sync` would do now; the timer runs it every sync_interval."),
		"",
	}, "\n")
}

func orDash(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func clamp(x, lo, hi int) int { return max(lo, min(x, hi)) }
