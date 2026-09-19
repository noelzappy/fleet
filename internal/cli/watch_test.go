package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/noelzappy/fleet/internal/config"
	"github.com/noelzappy/fleet/internal/fleetsync"
	"github.com/noelzappy/fleet/internal/ui"
)

func watchFleet() *config.Fleet {
	f := &config.Fleet{
		Waves:     []config.Wave{{Name: "backend"}},
		Harnesses: map[string]config.Harness{"cc": {Kind: "claude-code"}, "cx": {Kind: "codex"}},
		Profiles: map[string]config.Profile{
			"impl": {Harness: "cc", Role: config.RoleImplementer, Concurrency: 1, Waves: []string{"backend"}},
		},
	}
	f.ApplyDefaults()
	return f
}

func TestBuildIssueRows(t *testing.T) {
	f := watchFleet()
	L := f.Labels
	open := func(n int, labels ...string) fleetsync.GHIssue {
		return fleetsync.GHIssue{Number: n, Title: fmt.Sprintf("issue %d", n), State: "OPEN", Labels: labels}
	}
	st := fleetsync.State{
		GH: []fleetsync.GHIssue{
			open(1, L.Ready, "wave:backend"),              // dispatchable, no task yet
			open(2, L.Ready),                              // no wave label: stalled
			open(3, L.Stuck),                              // stuck
			open(4, L.NeedsHuman),                         // needs a human
			open(5, "wave:backend"),                       // mirrored, agent running
			open(6, "wave:backend"),                       // mirrored, has a PR with a green gate
			open(7, "wave:backend"),                       // mirrored, PR with a red gate
			{Number: 8, Title: "closed", State: "CLOSED"}, // never shown
			open(9),               // nothing to do with fleet
			open(10, L.BlockedBy), // waiting on deps
		},
		Multica: []fleetsync.MIssue{
			{Kind: fleetsync.KindTask, Issue: 5, Profile: "impl", Status: "in_progress", RunActive: true},
			{Kind: fleetsync.KindTask, Issue: 6, Profile: "impl", Status: "in_progress"},
			{Kind: fleetsync.KindTask, Issue: 7, Profile: "impl", Status: "in_progress"},
		},
		PRs: []fleetsync.PR{
			{Number: 60, Issue: 6, Gate: []fleetsync.GateRun{{Conclusion: "success"}}},
			{Number: 70, Issue: 7, Gate: []fleetsync.GateRun{{Conclusion: "failure", FailedJobs: []string{"lint"}}}},
		},
	}
	rows := buildIssueRows(f, st)
	got := map[int]issueRow{}
	for _, r := range rows {
		got[r.Num] = r
	}
	want := map[int]struct {
		state string
		tone  tone
	}{
		1: {"ready", toneAccent}, 2: {"ready · stalled", toneWarn}, 3: {"stuck", toneBad},
		4: {L.NeedsHuman, toneWarn}, 5: {"working", toneInfo}, 6: {"in review", toneGood},
		7: {"gate failing", toneBad}, 9: {"open", toneDim}, 10: {"waiting on deps", toneDim},
	}
	if len(got) != len(want) {
		t.Fatalf("rows for %d issues, want %d (closed issues must be absent): %+v", len(got), len(want), rows)
	}
	for n, w := range want {
		if got[n].State != w.state || got[n].Tone != w.tone {
			t.Errorf("#%d = %q/%v, want %q/%v", n, got[n].State, got[n].Tone, w.state, w.tone)
		}
	}
	if got[2].Why == "" || !strings.Contains(got[2].Why, "wave:backend") {
		t.Errorf("#2 should say which label is missing, got %q", got[2].Why)
	}
	if got[5].Agent != "impl ▶" || got[6].PR != 60 || got[6].Gate != "✓ pass" || got[7].Gate != "✗ lint" {
		t.Errorf("agent/PR/gate columns wrong: %+v %+v %+v", got[5], got[6], got[7])
	}
	// What needs a human sorts before active work, before the queue.
	first := rows[0].Tone
	for _, r := range rows[1:] {
		if r.Tone < first {
			t.Errorf("rows aren't sorted by urgency: %+v", rows)
			break
		}
		first = r.Tone
	}
}

func TestHarnessRows(t *testing.T) {
	f := watchFleet()
	rows := buildHarnessRows(f, fleetsync.State{
		SignedOut: map[string]bool{"cx": true},
		Cooling:   map[string]time.Time{"cc": time.Date(2026, 9, 19, 17, 30, 0, 0, time.Local)},
	})
	if len(rows) != 2 || rows[0].Name != "cc" || rows[1].Name != "cx" {
		t.Fatalf("rows = %+v", rows)
	}
	if !strings.HasPrefix(rows[0].Status, "out of quota until 17:30") || rows[0].Tone != toneWarn {
		t.Errorf("cooling row = %+v", rows[0])
	}
	if rows[1].Status != "signed out" || rows[1].Tone != toneBad {
		t.Errorf("signed-out row = %+v", rows[1])
	}
}

func syntheticSnapshot() *snapshot {
	f := watchFleet()
	var gh []fleetsync.GHIssue
	for i := 1; i <= 40; i++ {
		gh = append(gh, fleetsync.GHIssue{Number: i, State: "OPEN", Title: strings.Repeat("a long issue title ", 6), Labels: []string{f.Labels.Ready}})
	}
	st := fleetsync.State{GH: gh, SignedOut: map[string]bool{"cx": true}, Notes: []string{"sync: harness cx is signed out"}}
	s := &snapshot{At: time.Now(), Took: 1200 * time.Millisecond, Project: "widgets", Repo: "o/widgets", Daemon: "active", Timer: "loaded",
		State: st, Plan: fleetsync.Plan(f, st), Issues: buildIssueRows(f, st), Harness: buildHarnessRows(f, st),
		PRs: []prRow{{Num: 5, Issue: 1, Head: "agent/1-thing", Gate: "✗ lint", Tone: toneBad, Flags: []string{"conflicts"}}}}
	for i := 0; i < 60; i++ {
		s.Logs[0] = append(s.Logs[0], fmt.Sprintf("sync: line %d", i), "→ curl trace line")
	}
	return s
}

func plain() ui.Palette { return ui.NewPalette(io.Discard) }

func TestRenderFitsTheTerminal(t *testing.T) {
	s := syntheticSnapshot()
	for _, size := range [][2]int{{80, 24}, {120, 40}, {200, 60}, {60, 20}, {100, 12}} {
		for _, zoom := range []bool{false, true} {
			v := viewState{W: size[0], H: size[1], Zoom: zoom, Focus: paneIssues, Interval: 10 * time.Second}
			out := render(s, v, plain(), false) + "\n" + footer(plain(), v)
			lines := strings.Split(out, "\n")
			if size[1] >= 20 && len(lines) > size[1] {
				t.Errorf("%dx%d zoom=%v: %d lines, terminal has %d", size[0], size[1], zoom, len(lines), size[1])
			}
			for i, l := range lines {
				if w := lipgloss.Width(l); w > size[0] {
					t.Errorf("%dx%d zoom=%v line %d is %d wide: %q", size[0], size[1], zoom, i, w, l)
				}
			}
		}
	}
}

func TestRenderContent(t *testing.T) {
	s := syntheticSnapshot()
	v := viewState{W: 120, H: 40, Interval: 10 * time.Second}
	out := render(s, v, plain(), false)
	for _, want := range []string{"widgets", "o/widgets", "daemon ● active", "cx signed out", "Issues (40)", "Pull requests (1)", "Next sync tick", "Activity · sync log", "↳ no wave label"} {
		if !strings.Contains(out, want) {
			t.Errorf("dashboard is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "→ curl trace") {
		t.Error("command traces shown without verbose")
	}
	v.Verbose = true
	v.Zoom, v.Focus = true, paneLog
	if out := render(s, v, plain(), false); !strings.Contains(out, "→ curl trace") {
		t.Error("verbose should show the traces")
	}

	// No open PRs / no plan read as placeholders, not as a count of one.
	empty := *s
	empty.PRs, empty.Plan = nil, nil
	if out := render(&empty, viewState{W: 120, H: 40}, plain(), false); !strings.Contains(out, "Pull requests (0)") || !strings.Contains(out, "no open pull requests") {
		t.Errorf("empty PR pane wrong:\n%s", out)
	}
	if out := render(nil, viewState{W: 80, H: 24}, plain(), false); !strings.Contains(out, "collecting") {
		t.Errorf("first paint = %q", out)
	}
	if out := render(s, viewState{W: 120, H: 40, Err: errors.New("gh: boom\nsecond line")}, plain(), false); !strings.Contains(out, "refresh failed") || strings.Count(out, "second line") != 1 {
		t.Errorf("refresh error not shown on one line:\n%s", out)
	}
}

func TestScrollingClamps(t *testing.T) {
	s := syntheticSnapshot()
	v := viewState{W: 120, H: 30, Zoom: true, Focus: paneIssues}
	v.Off[paneIssues] = 1 << 30 // "G": far past the end
	out := render(s, v, plain(), false)
	if !strings.Contains(out, "#40") || strings.Contains(out, "#1 ") {
		t.Errorf("scrolling to the end should show the last issue, not the first:\n%s", out)
	}
	if !strings.Contains(out, "of 81") { // header + 40 issues + 40 "↳ why stalled" lines
		t.Errorf("scrolled pane should show its line range:\n%s", out)
	}
	v.Off[paneIssues] = -5
	if out := render(s, v, plain(), false); !strings.Contains(out, "#1 ") {
		t.Error("negative offset should clamp to the top")
	}
}

func TestWatchKeys(t *testing.T) {
	m := newWatchModel(10 * time.Second)
	m.W, m.H = 100, 30
	m.Loading = false
	press := func(k string) tea.Cmd {
		var msg tea.KeyMsg
		switch k {
		case "tab":
			msg = tea.KeyMsg{Type: tea.KeyTab}
		case "shift+tab":
			msg = tea.KeyMsg{Type: tea.KeyShiftTab}
		case "down":
			msg = tea.KeyMsg{Type: tea.KeyDown}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		_, cmd := m.Update(msg)
		return cmd
	}
	press("tab")
	press("tab")
	if m.Focus != panePlan {
		t.Errorf("focus after 2 tabs = %v", m.Focus)
	}
	press("shift+tab")
	press("shift+tab")
	press("shift+tab")
	if m.Focus != paneLog {
		t.Errorf("shift-tab should wrap to the last pane, got %v", m.Focus)
	}
	press("down") // the log follows its tail: down can't go past the newest line
	if m.LogBack != 0 {
		t.Errorf("LogBack = %d, want 0 while following", m.LogBack)
	}
	press("up")
	press("k")
	if m.LogBack != 2 {
		t.Errorf("scrolling the log up 2 lines: LogBack = %d", m.LogBack)
	}
	press("down")
	if m.LogBack != 1 {
		t.Errorf("down moves toward the tail: LogBack = %d", m.LogBack)
	}
	press("g")
	if m.LogBack < 1000 {
		t.Error("g should jump to the oldest line")
	}
	press("G")
	if m.LogBack != 0 {
		t.Error("G should resume following the tail")
	}
	press("z")
	press("l")
	press("v")
	if !m.Zoom || m.LogSel != 1 || !m.Verbose {
		t.Errorf("toggles not applied: %+v", m.viewState)
	}
	press("?")
	if !m.help || !strings.Contains(m.View(), "read-only") {
		t.Error("help screen missing")
	}
	if cmd := press("q"); cmd != nil { // q closes help first, it does not quit
		t.Error("q in help must close help, not quit")
	}
	if press("r") == nil {
		t.Error("r should start a refresh when idle")
	}
	if !m.Loading {
		t.Error("refresh should mark the model loading")
	}
	if press("r") != nil {
		t.Error("r while a refresh is running must not stack another")
	}
	if cmd := press("q"); cmd == nil {
		t.Error("q should quit")
	}
}

func TestWatchSurvivesAZeroSizeTerminal(t *testing.T) {
	m := newWatchModel(10 * time.Second)
	m.Update(tea.WindowSizeMsg{Width: 0, Height: 0})
	m.Update(snapMsg{s: syntheticSnapshot()})
	if v := m.View(); !strings.Contains(v, "Issues") {
		t.Errorf("a 0x0 terminal should fall back to 80x24, got %q", v)
	}
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 50})
	if m.W != 140 || m.H != 50 {
		t.Errorf("a real size must win: %dx%d", m.W, m.H)
	}
}

func TestWatchUpdateKeepsLastGoodSnapshot(t *testing.T) {
	m := newWatchModel(10 * time.Second)
	m.W, m.H = 100, 30
	good := syntheticSnapshot()
	m.Update(snapMsg{s: good})
	if m.snap != good || m.Err != nil || m.Loading {
		t.Fatalf("after a good refresh: %+v", m.viewState)
	}
	m.Update(snapMsg{err: errors.New("gh: rate limited")})
	if m.snap != good || m.Err == nil {
		t.Error("a failed refresh must keep the last good data and record the error")
	}
	m.Update(snapMsg{s: good})
	if m.Err != nil {
		t.Error("a good refresh should clear the error")
	}
	// A tick during a slow refresh doesn't start a second one.
	m.Loading = true
	if _, cmd := m.Update(tickMsg(time.Now())); cmd == nil || !m.Loading {
		t.Error("tick while loading should just re-arm the timer")
	}
}

func TestTailFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "log")
	os.WriteFile(p, []byte("one\ntwo\nthree\nfour\n"), 0o644)
	if got := tailFile(p, 1<<20); got != "one\ntwo\nthree\nfour\n" {
		t.Errorf("whole file = %q", got)
	}
	// A cut that lands mid-line drops the partial first line.
	if got := tailFile(p, 12); got != "three\nfour\n" {
		t.Errorf("tail = %q", got)
	}
	if got := tailFile(filepath.Join(t.TempDir(), "missing"), 100); got != "" {
		t.Errorf("missing file = %q", got)
	}
}

func TestSnapshotCapsTheLog(t *testing.T) {
	s := syntheticSnapshot() // 60 non-trace log lines
	out := render(s, viewState{W: 120, Interval: time.Second}, plain(), true)
	if n := strings.Count(out, "sync: line "); n != snapshotLogLines {
		t.Errorf("--once printed %d log lines, want the last %d", n, snapshotLogLines)
	}
	if !strings.Contains(out, "sync: line 59") || strings.Contains(out, "sync: line 44\n") {
		t.Errorf("should keep the newest lines:\n%s", out)
	}
}

func TestHeightsRedistribute(t *testing.T) {
	v := viewState{W: 120, H: 40}
	const headerFooter = 4
	avail := v.H - headerFooter - int(numPanes)
	// Two panes are nearly empty: their unused lines go to the ones that overflow.
	lens := [numPanes]int{60, 1, 1, 60}
	h, _ := v.heights(headerFooter, lens)
	if h[panePRs] != 1 || h[panePlan] != 1 {
		t.Errorf("empty panes keep only what they need: %v", h)
	}
	sum := 0
	for _, x := range h {
		sum += x
	}
	if sum != avail {
		t.Errorf("heights %v use %d lines, want all %d", h, sum, avail)
	}
	if h[paneIssues] <= h[paneLog]-1 && h[paneIssues] < 10 {
		t.Errorf("overflowing panes should share the surplus: %v", h)
	}
	// When everything overflows, nothing exceeds the space and the split follows the weights.
	all := [numPanes]int{99, 99, 99, 99}
	h, _ = v.heights(headerFooter, all)
	sum = 0
	for _, x := range h {
		sum += x
	}
	if sum > avail || h[paneIssues] < h[panePRs] {
		t.Errorf("weighted split wrong: %v (avail %d)", h, avail)
	}
	// A pane never gets more than it has lines.
	small := [numPanes]int{3, 3, 3, 3}
	if h, _ := v.heights(headerFooter, small); h != small {
		t.Errorf("heights %v exceed content %v", h, small)
	}
}

func TestLogFollowsTheTail(t *testing.T) {
	s := syntheticSnapshot() // sync: line 0 … sync: line 59
	v := viewState{W: 120, H: 20, Zoom: true, Focus: paneLog}
	out := render(s, v, plain(), false)
	if !strings.Contains(out, "sync: line 59") || strings.Contains(out, "sync: line 0\n") {
		t.Errorf("the log should open on its newest lines:\n%s", out)
	}
	v.LogBack = 1 << 30
	out = render(s, v, plain(), false)
	if !strings.Contains(out, "sync: line 0") || strings.Contains(out, "sync: line 59") {
		t.Errorf("scrolled to the top it should show the oldest lines:\n%s", out)
	}
	if !strings.Contains(out, "G to follow") {
		t.Errorf("a scrolled-back log should say how to resume:\n%s", out)
	}
	v.LogBack = 0
	if out := render(s, v, plain(), false); strings.Contains(out, "G to follow") {
		t.Error("a following log has nothing to resume")
	}
}

func TestIdleRowExplainsWhatHappensNext(t *testing.T) {
	f := watchFleet()
	now := time.Date(2026, 9, 19, 14, 30, 0, 0, time.UTC)
	ended := now.Add(-11 * time.Minute)
	task := func(mut func(*fleetsync.MIssue)) fleetsync.State {
		m := fleetsync.MIssue{Kind: fleetsync.KindTask, Issue: 7, Profile: "impl", Status: "in_progress",
			LastRunID: "r1", LastRunStatus: "completed", LastRunEnded: ended}
		if mut != nil {
			mut(&m)
		}
		return fleetsync.State{Now: now, GH: []fleetsync.GHIssue{{Number: 7, State: "OPEN", Title: "t", Labels: []string{"wave:backend"}}}, Multica: []fleetsync.MIssue{m}}
	}
	cases := []struct {
		name string
		mut  func(*fleetsync.MIssue)
		why  string
	}{
		{"first idle end", nil, "reminds it once"},
		{"handled, awaiting the next run", func(m *fleetsync.MIssue) { m.IdleNudgedRun, m.IdleNudges = "r1", 1 }, "ended 11m0s ago with no PR"},
		{"ended again after a reminder", func(m *fleetsync.MIssue) { m.LastRunID, m.IdleNudgedRun, m.IdleNudges = "r2", "r1", 1 }, "next tick escalates"},
		{"escalated", func(m *fleetsync.MIssue) { m.IdleEsc = "r1" }, "Remove the needs-human label"},
	}
	for _, c := range cases {
		rows := buildIssueRows(f, task(c.mut))
		if len(rows) != 1 || rows[0].State != "idle · no PR" || rows[0].Tone != toneBad || !strings.Contains(rows[0].Why, c.why) {
			t.Errorf("%s: %+v, want an idle row whose reason contains %q", c.name, rows, c.why)
		}
	}
	// Within the grace period it is just queued work, not a problem.
	fresh := task(func(m *fleetsync.MIssue) { m.LastRunEnded = now.Add(-time.Minute) })
	if rows := buildIssueRows(f, fresh); rows[0].State == "idle · no PR" {
		t.Errorf("flagged idle inside the grace period: %+v", rows[0])
	}
}
