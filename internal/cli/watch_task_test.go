package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/noelzappy/fleet/internal/config"
	"github.com/noelzappy/fleet/internal/fleetsync"
	"github.com/noelzappy/fleet/internal/shell"
)

var tNow = time.Date(2026, 9, 19, 14, 40, 0, 0, time.UTC)

func sampleDetail() *taskDetail {
	return &taskDetail{
		Num: 7, Title: "[CONTRACTS] app auth + me: PIN reset and devices", URL: "https://github.com/o/r/issues/7",
		Body: "Define the contract for the app auth and /me endpoints.\n\n## Acceptance\n- OpenAPI + types",
		Row:  issueRow{Num: 7, State: "idle · no PR", Tone: toneBad, Why: "ended 11m ago with no PR; the next tick reminds it once"},
		Task: fleetsync.MIssue{ID: "m7", Kind: fleetsync.KindTask, Issue: 7, Profile: "impl-g", Status: "in_progress"}, HasTask: true,
		Runs: []runInfo{
			{ID: "r2", Status: "running", Started: tNow.Add(-2 * time.Minute)},
			{ID: "r1", Status: "completed", Started: tNow.Add(-35 * time.Minute), Ended: tNow.Add(-30 * time.Minute)},
			{ID: "r0", Status: "failed", Error: "codex: 401 unauthorized", Started: tNow.Add(-50 * time.Minute), Ended: tNow.Add(-49 * time.Minute)},
		},
		Comments: []commentInfo{
			{Author: "agent", At: tNow.Add(-33 * time.Minute), Content: "I am running `pnpm gate` and will wait for it in the background."},
			{Author: "member", At: tNow.Add(-3 * time.Minute), Content: "You stopped early. Run the gate in the foreground."},
		},
		PR: &prRow{Num: 23, Issue: 7, Head: "agent/7-contracts", Gate: "✗ lint", Tone: toneBad, Flags: []string{"conflicts"}},
		Work: worktreeInfo{Path: "/runs/payl-1/workdir/repo", Branch: "agent/7-contracts", Shortstat: "4 files changed, 262 insertions(+), 1 deletion(-)",
			Changed: []string{" M openapi/app.yaml", "?? src/new.spec.ts"}, Log: []string{"b559a77 chore: fleet setup"}},
	}
}

func TestParseRunsAndComments(t *testing.T) {
	runs := parseRuns(`[
	  {"id":"a","status":"completed","started_at":"2026-09-19T14:05:03Z","completed_at":"2026-09-19T14:10:07Z","work_dir":"/w/a","error":null},
	  {"id":"b","status":"running","started_at":"2026-09-19T14:24:02Z","completed_at":null,"attempt":2}]`)
	if len(runs) != 2 || runs[0].ID != "b" || runs[1].ID != "a" {
		t.Fatalf("runs should sort newest first: %+v", runs)
	}
	if runs[1].WorkDir != "/w/a" || runs[1].Ended.Sub(runs[1].Started) != 5*time.Minute+4*time.Second || runs[0].Attempt != 2 || !runs[0].Ended.IsZero() {
		t.Errorf("run fields wrong: %+v", runs)
	}
	if runs[1].Error != "" {
		t.Errorf("a null error must read as empty, got %q", runs[1].Error)
	}
	cs := parseComments(`[
	  {"author_type":"agent","content":"  second  ","created_at":"2026-09-19T14:28:31Z"},
	  {"author_type":"member","content":"first","created_at":"2026-09-19T14:20:00Z"}]`)
	if len(cs) != 2 || cs[0].Content != "first" || cs[0].Author != "member" || cs[1].Content != "second" {
		t.Errorf("comments should be oldest first, trimmed: %+v", cs)
	}
	if parseRuns("not json") != nil || parseComments("") != nil {
		t.Error("garbage should parse to nothing, not panic")
	}
}

func TestFindGitDir(t *testing.T) {
	root := t.TempDir()
	mk := func(p string) { os.MkdirAll(filepath.Join(root, p), 0o755) }
	mk("run1/workdir/paylte-platform/.git")
	mk("run2/.git")
	mk("run3/a/b/c/.git") // too deep
	mk("run4/workdir/node_modules/x/.git")
	mk("run4/workdir/.hidden/.git")
	if got := findGitDir(filepath.Join(root, "run1")); got != filepath.Join(root, "run1/workdir/paylte-platform") {
		t.Errorf("Multica's layout: got %q", got)
	}
	if got := findGitDir(filepath.Join(root, "run2")); got != filepath.Join(root, "run2") {
		t.Errorf("the dir itself: got %q", got)
	}
	for _, r := range []string{"run3", "run4", "missing"} {
		if got := findGitDir(filepath.Join(root, r)); got != "" {
			t.Errorf("%s: found %q", r, got)
		}
	}
}

func TestAskPromptCarriesTheContext(t *testing.T) {
	td := sampleDetail()
	thread := []exchange{
		{Kind: exAsk, Text: "why did it stop?"},
		{Kind: exAnswer, Text: "It backgrounded the gate."},
		{Kind: exAnswer, Text: "boom", Bad: true}, // a failed answer is not history
		{Kind: exTell, Text: "run the gate in the foreground"},
		{Kind: exNote, Text: "Sent."},
	}
	got := askPrompt(td, thread, "  and what is left uncommitted?  ", tNow)
	for _, want := range []string{
		"no tools", "#7", "PIN reset", "idle · no PR", "impl-g", "in_progress", "Define the contract",
		"running", "completed", "codex: 401 unauthorized", "pnpm gate", "[owner ", "PULL REQUEST #23", "✗ lint", "conflicts",
		"branch agent/7-contracts", "262 insertions", " M openapi/app.yaml", "b559a77",
		"Owner asked: why did it stop?", "You answered: It backgrounded the gate.", "Owner told the agent: run the gate in the foreground",
		"QUESTION: and what is left uncommitted?",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt is missing %q", want)
		}
	}
	if strings.Contains(got, "boom") || strings.Contains(got, "Sent.") {
		t.Error("failed answers and system notes must not be fed back as conversation")
	}
	if !strings.HasSuffix(strings.TrimSpace(got), "and what is left uncommitted?") {
		t.Error("the question should come last")
	}
	// It stays bounded however large the inputs are.
	td.Body = strings.Repeat("x", 100000)
	td.Comments[0].Content = strings.Repeat("y", 100000)
	if n := len(askPrompt(td, nil, "q", tNow)); n > 12000 {
		t.Errorf("prompt is %d bytes; it should stay small", n)
	}
	// An undispatched task and a missing checkout say so instead of inventing detail.
	bare := &taskDetail{Num: 9, Title: "t", Row: issueRow{State: "open"}}
	p := askPrompt(bare, nil, "q", tNow)
	for _, want := range []string{"has not been dispatched", "No pull request yet", noChangesInWorktr} {
		if !strings.Contains(p, want) {
			t.Errorf("bare prompt missing %q:\n%s", want, p)
		}
	}
}

func TestCanTell(t *testing.T) {
	td := sampleDetail()
	if ok, _ := canTell(td); !ok {
		t.Error("an in-progress task can be told something")
	}
	td.Task.Status = fleetsync.StatusBlocked
	if ok, _ := canTell(td); !ok {
		t.Error("a blocked task can be answered")
	}
	td.Task.Status = fleetsync.StatusDone
	if ok, why := canTell(td); ok || !strings.Contains(why, "done") {
		t.Errorf("a done task can't: %v %q", ok, why)
	}
	if ok, why := canTell(&taskDetail{Num: 9}); ok || !strings.Contains(why, "hasn't been dispatched") {
		t.Errorf("an undispatched issue has no agent: %v %q", ok, why)
	}
}

// fakeMultica puts a `multica` on PATH that records each call's arguments and stdin.
func fakeMultica(t *testing.T) (calls func() []string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + log + "'\nif [ \"$3\" = add ] || [ \"$4\" = add ]; then printf 'STDIN<%s>\\n' \"$(cat)\" >> '" + log + "'; fi\n"
	if err := os.WriteFile(filepath.Join(dir, "multica"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := shell.PathPrefix
	shell.PathPrefix = dir
	t.Cleanup(func() { shell.PathPrefix = old })
	return func() []string {
		b, _ := os.ReadFile(log)
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
}

func TestSendFollowUp(t *testing.T) {
	calls := fakeMultica(t)
	ctx := context.Background()
	td := sampleDetail()
	if err := sendFollowUp(ctx, td, "  run the gate in the foreground  "); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(calls(), "\n")
	if !strings.Contains(got, "issue comment add m7 --content-stdin") {
		t.Errorf("should comment on the Multica task:\n%s", got)
	}
	if !strings.Contains(got, "STDIN<@impl-g Follow-up from the owner (sent from fleet watch):\n\nrun the gate in the foreground>") {
		t.Errorf("the comment should mention the agent and carry the trimmed text:\n%s", got)
	}
	if strings.Contains(got, "issue status") {
		t.Errorf("an in-progress task must not be re-queued:\n%s", got)
	}

	// A blocked task is unblocked too, as sync does when you answer on GitHub.
	td.Task.Status = fleetsync.StatusBlocked
	if err := sendFollowUp(ctx, td, "use the second option"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(calls(), "\n"); !strings.Contains(got, "issue status m7 todo") {
		t.Errorf("a blocked task should go back to todo:\n%s", got)
	}

	// Refusals make no call at all.
	before := len(calls())
	for name, d := range map[string]*taskDetail{"undispatched": {Num: 9}, "done": func() *taskDetail { x := sampleDetail(); x.Task.Status = fleetsync.StatusDone; return x }()} {
		if err := sendFollowUp(ctx, d, "hi"); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if err := sendFollowUp(ctx, sampleDetail(), "   "); err == nil {
		t.Error("an empty message must not be sent")
	}
	if len(calls()) != before {
		t.Errorf("a refused send still called multica: %v", calls()[before:])
	}
}

func TestPickAskHarness(t *testing.T) {
	old := cfg
	defer func() { cfg = old }()
	cfg = &config.Fleet{Harnesses: map[string]config.Harness{
		"claude-code": {Kind: "claude-code"}, "codex": {Kind: "codex"}, "agy": {Kind: "antigravity"}, "opencode": {Kind: "opencode"}}}
	name, build, err := pickAskHarness(nil)
	if err != nil || name != "claude-code" {
		t.Fatalf("claude-code first: %q %v", name, err)
	}
	cmd := build("hello 'world'")
	for _, want := range []string{"claude -p", "--tools ''", "--no-session-persistence", "'hello '\\''world'\\'''"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("ask command missing %q: %s", want, cmd)
		}
	}
	if strings.Contains(cmd, "--allowedTools") || strings.Contains(cmd, "dangerously") {
		t.Errorf("an ask must never grant tools: %s", cmd)
	}
	if name, build, _ := pickAskHarness(map[string]bool{"claude-code": true}); name != "codex" || !strings.Contains(build("q"), "--sandbox read-only") {
		t.Errorf("fall back to codex in a read-only sandbox, got %q", name)
	}
	if _, _, err := pickAskHarness(map[string]bool{"claude-code": true, "codex": true}); err == nil {
		t.Error("nothing signed in can answer: expect an error")
	}
	cfg = &config.Fleet{Harnesses: map[string]config.Harness{"agy": {Kind: "antigravity"}, "opencode": {Kind: "opencode"}}}
	if _, _, err := pickAskHarness(nil); err == nil {
		t.Error("harnesses with no way to disable tools must not be used to ask")
	}
}

// ---- the task screen ----------------------------------------------------------------

func TestRenderTaskFitsAndShowsWhatMatters(t *testing.T) {
	td := sampleDetail()
	tv := &taskView{Detail: td, Now: tNow, Input: "why did it stop?", Harness: "claude-code", Thread: []exchange{
		{Kind: exAsk, Text: "why did it stop?"},
		{Kind: exAnswer, Text: "Its last run completed after it started `pnpm gate` in the background, so the gate never finished and nothing was committed."},
		{Kind: exTell, Text: "run the gate in the foreground"},
		{Kind: exNote, Text: "Sent."},
	}}
	for _, size := range [][2]int{{80, 24}, {120, 40}, {200, 60}, {60, 20}} {
		tv.W, tv.H = size[0], size[1]
		out := renderTask(tv, plain())
		lines := strings.Split(out, "\n")
		if len(lines) > size[1] {
			t.Errorf("%dx%d: %d lines", size[0], size[1], len(lines))
		}
		for i, l := range lines {
			if w := lipgloss.Width(l); w > size[0] {
				t.Errorf("%dx%d line %d is %d wide: %q", size[0], size[1], i, w, l)
			}
		}
	}
	tv.W, tv.H = 130, 60
	out := renderTask(tv, plain())
	for _, want := range []string{"#7", "PIN reset", "idle · no PR", "agent impl-g", "PR #23", "ended 11m ago",
		"running", "completed", "failed", "codex: 401 unauthorized", "you ", "You stopped early", "pnpm gate",
		"#23 agent/7-contracts", "✗ lint", "branch agent/7-contracts", "262 insertions", "2 uncommitted", " M openapi/app.yaml",
		"you › why did it stop?", "fleet › Its last run completed", "you → impl-g ✓ run the gate in the foreground", "ask ▸ why did it stop?"} {
		if !strings.Contains(out, want) {
			t.Errorf("task screen is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "member") {
		t.Error("the owner's comments should read 'you', not 'member'")
	}

	tv.Intent = intentTell
	if out := renderTask(tv, plain()); !strings.Contains(out, "tell impl-g ▸ ") {
		t.Error("the prompt should name who you are telling")
	}
	tv.Confirm = "Send to impl-g now?"
	out = renderTask(tv, plain())
	if !strings.Contains(out, "Send to impl-g now?") || strings.Contains(out, "tell impl-g ▸ ") || !strings.Contains(out, "enter send") {
		t.Errorf("a pending confirmation replaces the input and the footer keys:\n%s", out)
	}
	tv.Confirm, tv.Busy = "", true
	if out := renderTask(tv, plain()); !strings.Contains(out, "thinking… (asking claude-code)") {
		t.Error("a question in flight should say so and who is answering")
	}
	tv.Err = errors.New("multica: boom\nsecond line")
	if out := renderTask(tv, plain()); !strings.Contains(out, "✗ multica: boom second line") {
		t.Errorf("errors show on one line:\n%s", out)
	}

	// Before anything is asked, the empty conversation explains the two modes.
	tv = &taskView{Detail: td, Now: tNow, W: 130, H: 40, Harness: "claude-code"}
	if out := renderTask(tv, plain()); !strings.Contains(out, "press tab to tell impl-g") || !strings.Contains(out, "can't change") {
		t.Errorf("the empty state should explain ask vs tell:\n%s", out)
	}
	tv = &taskView{Detail: td, Now: tNow, W: 130, H: 40}
	if out := renderTask(tv, plain()); !strings.Contains(out, "No signed-in claude-code or codex harness") {
		t.Errorf("with nothing able to answer, say so:\n%s", out)
	}
	// An undispatched issue can only be asked about, and says why.
	tv = &taskView{Detail: &taskDetail{Num: 9, Title: "t", Row: issueRow{State: "open"}}, Now: tNow, W: 130, H: 40}
	if out := renderTask(tv, plain()); !strings.Contains(out, "hasn't been dispatched") || !strings.Contains(out, "Not dispatched") {
		t.Errorf("undispatched:\n%s", out)
	}
}

// ---- the model: keys and commands ---------------------------------------------------

type recorder struct {
	fetches int
	loads   int
	asked   []string
	sent    []string
	answer  string
	askErr  error
	sendEr  error
}

func (r *recorder) ops() watchOps {
	return watchOps{
		harness: func(out map[string]bool) string {
			if out["claude-code"] {
				return ""
			}
			return "claude-code"
		},
		collect: func(context.Context) (*snapshot, error) {
			r.fetches++
			return syntheticSnapshot(), nil
		},
		load: func(_ context.Context, s *snapshot, n int) (*taskDetail, error) {
			r.loads++
			td := sampleDetail()
			td.Num = n
			return td, nil
		},
		ask: func(_ context.Context, _ map[string]bool, prompt string) (string, string, error) {
			r.asked = append(r.asked, prompt)
			return r.answer, "claude-code", r.askErr
		},
		send: func(_ context.Context, _ *taskDetail, text string) error {
			r.sent = append(r.sent, text)
			return r.sendEr
		},
	}
}

// runCmd executes a command and feeds its messages back, skipping timers and blinks.
func runCmd(m *watchModel, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	var msg tea.Msg
	select {
	case msg = <-done:
	case <-time.After(300 * time.Millisecond):
		return // a tea.Tick: not what these tests are about
	}
	switch v := msg.(type) {
	case tea.BatchMsg:
		for _, c := range v {
			runCmd(m, c)
		}
	case snapMsg, detailMsg, answerMsg, sentMsg:
		_, next := m.Update(v)
		runCmd(m, next)
	}
}

func key(m *watchModel, s string) tea.Cmd {
	var msg tea.KeyMsg
	switch s {
	case "enter":
		msg = tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		msg = tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		msg = tea.KeyMsg{Type: tea.KeyTab}
	case "down":
		msg = tea.KeyMsg{Type: tea.KeyDown}
	case "up":
		msg = tea.KeyMsg{Type: tea.KeyUp}
	default:
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
	_, cmd := m.Update(msg)
	return cmd
}

func typeText(m *watchModel, s string) {
	for _, r := range s {
		key(m, string(r))
	}
}

func taskModel(t *testing.T) (*watchModel, *recorder) {
	t.Helper()
	rec := &recorder{answer: "It backgrounded the gate."}
	m := newWatchModel(10 * time.Second)
	m.W, m.H = 120, 40
	m.ops = rec.ops()
	s := syntheticSnapshot()
	s.State.Multica = []fleetsync.MIssue{{ID: "m1", Kind: fleetsync.KindTask, Issue: 1, Profile: "impl-g", Status: "in_progress"}}
	m.Update(snapMsg{s: s})
	return m, rec
}

func TestBoardCursorMovesBetweenIssues(t *testing.T) {
	m, _ := taskModel(t)
	if m.snap.Issues[0].Num != 1 {
		t.Fatalf("fixture: first row is #%d", m.snap.Issues[0].Num)
	}
	key(m, "tab") // focus stays on Issues (pane 0) until tabbed; tab moves it, so go back
	key(m, "shift+tab")
	m.Focus = paneIssues
	key(m, "j")
	key(m, "j")
	if m.SelNum != 3 {
		t.Errorf("two downs from the top should select #3, got #%d", m.SelNum)
	}
	key(m, "k")
	if m.SelNum != 2 {
		t.Errorf("up: #%d", m.SelNum)
	}
	key(m, "G")
	if m.SelNum != 40 {
		t.Errorf("G should select the last issue, got #%d", m.SelNum)
	}
	key(m, "down") // past the end stays on the last row
	if m.SelNum != 40 {
		t.Errorf("cursor ran off the list: #%d", m.SelNum)
	}
	key(m, "g")
	if m.SelNum != 1 {
		t.Errorf("g should select the first: #%d", m.SelNum)
	}
	// The cursor is an issue number, so it survives the list re-sorting under it.
	m.SelNum = 5
	s := syntheticSnapshot()
	s.Issues[0], s.Issues[4] = s.Issues[4], s.Issues[0]
	m.Update(snapMsg{s: s})
	if got := m.snap.Issues[selectedIssue(m.snap, m.SelNum)].Num; got != 5 {
		t.Errorf("after a re-sort the cursor moved to #%d", got)
	}
}

func TestOpenAskAndAnswer(t *testing.T) {
	m, rec := taskModel(t)
	m.Focus = paneIssues
	runCmd(m, key(m, "enter"))
	if m.mode != modeTask || m.tv.Detail.Num != 1 {
		t.Fatalf("enter should open the task under the cursor: mode=%v detail=%+v", m.mode, m.tv.Detail)
	}
	if m.tv.Harness != "claude-code" || !strings.Contains(m.View(), "sends the context above to claude-code") {
		t.Errorf("the view should name who answers before anything is asked: %q", m.tv.Harness)
	}
	if rec.loads != 1 || !m.detailLoaded || len(m.tv.Detail.Runs) == 0 {
		t.Errorf("the full detail should load once and replace the quick one: loads=%d loaded=%v", rec.loads, m.detailLoaded)
	}

	typeText(m, "why did it stop?")
	runCmd(m, key(m, "enter"))
	if len(rec.asked) != 1 || !strings.Contains(rec.asked[0], "QUESTION: why did it stop?") || !strings.Contains(rec.asked[0], "no tools") {
		t.Fatalf("the question should go to the model with the task context: %v", rec.asked)
	}
	th := m.tv.Thread
	if len(th) != 2 || th[0].Kind != exAsk || th[1].Kind != exAnswer || th[1].Text != "It backgrounded the gate." || m.tv.Busy {
		t.Errorf("thread = %+v busy=%v", th, m.tv.Busy)
	}
	if m.input.Value() != "" {
		t.Errorf("the box should clear after sending, has %q", m.input.Value())
	}
	if len(rec.sent) != 0 {
		t.Error("asking must never send anything to an agent")
	}

	// A follow-up question carries the earlier exchange.
	typeText(m, "and then?")
	runCmd(m, key(m, "enter"))
	if len(rec.asked) != 2 || !strings.Contains(rec.asked[1], "Owner asked: why did it stop?") || !strings.Contains(rec.asked[1], "You answered: It backgrounded the gate.") {
		t.Errorf("follow-ups should see the conversation so far:\n%s", rec.asked[len(rec.asked)-1])
	}

	// A failing model shows the error in the conversation and keeps going.
	rec.askErr = errors.New("claude-code couldn't answer: exit 1")
	typeText(m, "again")
	runCmd(m, key(m, "enter"))
	last := m.tv.Thread[len(m.tv.Thread)-1]
	if !last.Bad || !strings.Contains(last.Text, "couldn't answer") || m.tv.Busy {
		t.Errorf("a failed ask: %+v busy=%v", last, m.tv.Busy)
	}

	// esc leaves the task and drops its conversation; the board is back.
	key(m, "esc")
	if m.mode != modeBoard || len(m.tv.Thread) != 0 {
		t.Errorf("esc should close the task: mode=%v thread=%d", m.mode, len(m.tv.Thread))
	}
	if !strings.Contains(m.View(), "Issues") {
		t.Error("the board should be showing again")
	}
}

func TestTellNeedsConfirmation(t *testing.T) {
	m, rec := taskModel(t)
	m.Focus = paneIssues
	runCmd(m, key(m, "enter"))

	key(m, "tab")
	if m.tv.Intent != intentTell {
		t.Fatal("tab should switch to tell")
	}
	typeText(m, "run the gate in the foreground")
	key(m, "enter")
	if len(rec.sent) != 0 {
		t.Fatal("enter must ask for confirmation first, not send")
	}
	if m.pendingTell != "run the gate in the foreground" || !strings.Contains(m.View(), "Send to impl-g now?") {
		t.Fatalf("a confirmation should be showing: pending=%q\n%s", m.pendingTell, m.View())
	}

	// While it waits, typing and tab do nothing, so what you confirm is what you read.
	typeText(m, "xyz")
	key(m, "tab")
	if m.pendingTell != "run the gate in the foreground" || m.tv.Intent != intentTell {
		t.Errorf("the pending message changed under the confirmation: %q", m.pendingTell)
	}

	// esc cancels the confirmation, not the whole task, and sends nothing.
	key(m, "esc")
	if m.pendingTell != "" || m.mode != modeTask || len(rec.sent) != 0 {
		t.Errorf("esc at the confirmation: pending=%q mode=%v sent=%v", m.pendingTell, m.mode, rec.sent)
	}

	// Confirm for real.
	key(m, "enter")
	if m.pendingTell == "" {
		t.Fatal("the typed text should still be there to confirm")
	}
	runCmd(m, key(m, "enter"))
	if len(rec.sent) != 1 || rec.sent[0] != "run the gate in the foreground" {
		t.Fatalf("sent = %v", rec.sent)
	}
	th := m.tv.Thread
	if len(th) < 2 || th[len(th)-2].Kind != exTell || th[len(th)-1].Kind != exNote || th[len(th)-1].Bad {
		t.Errorf("the conversation should record what was sent: %+v", th)
	}
	if m.tv.Intent != intentAsk || m.pendingTell != "" || m.input.Value() != "" {
		t.Errorf("after sending we're back to asking with an empty box: intent=%v pending=%q input=%q", m.tv.Intent, m.pendingTell, m.input.Value())
	}

	// A failed send says so and doesn't pretend.
	rec.sendEr = errors.New("multica comment: 500")
	key(m, "tab")
	typeText(m, "again")
	key(m, "enter")
	runCmd(m, key(m, "enter"))
	last := m.tv.Thread[len(m.tv.Thread)-1]
	if !last.Bad || !strings.Contains(last.Text, "not sent") {
		t.Errorf("failed send: %+v", last)
	}
	for _, e := range m.tv.Thread[len(m.tv.Thread)-1:] {
		if e.Kind == exTell {
			t.Error("a failed send was recorded as sent")
		}
	}
}

func TestTellIsRefusedWhereThereIsNoAgent(t *testing.T) {
	m, rec := taskModel(t)
	m.Focus = paneIssues
	key(m, "j") // #2 has no Multica task
	runCmd(m, key(m, "enter"))
	if m.tv.Detail.Num != 2 {
		t.Fatalf("opened #%d", m.tv.Detail.Num)
	}
	m.tv.Detail.HasTask = false // the stub loader always returns a task; #2 really has none
	key(m, "tab")
	if m.tv.Intent != intentAsk || m.tv.Err == nil || !strings.Contains(m.tv.Err.Error(), "hasn't been dispatched") {
		t.Errorf("tab on an undispatched issue should refuse, with the reason: intent=%v err=%v", m.tv.Intent, m.tv.Err)
	}
	if len(rec.sent) != 0 {
		t.Error("nothing to send to")
	}
	// Asking still works there.
	typeText(m, "why isn't this running?")
	runCmd(m, key(m, "enter"))
	if len(rec.asked) != 1 {
		t.Errorf("asking about an undispatched issue should still work: %v", rec.asked)
	}
}

func TestTaskReloadsWithTheBoard(t *testing.T) {
	m, rec := taskModel(t)
	m.Focus = paneIssues
	runCmd(m, key(m, "enter"))
	base := rec.loads
	_, cmd := m.Update(tickMsg(time.Now()))
	m.Loading = true // the board refresh is in flight; only the task reload matters here
	runCmd(m, cmd)
	if rec.loads != base+1 {
		t.Errorf("an open task should reload on the refresh tick: %d -> %d", base, rec.loads)
	}
	// A slow reload isn't stacked.
	m.loadingTask = true
	_, cmd = m.Update(tickMsg(time.Now()))
	runCmd(m, cmd)
	if rec.loads != base+1 {
		t.Error("stacked a reload while one was running")
	}
	// A stale detail for a task that is no longer open is dropped.
	m.loadingTask = false
	m.Update(detailMsg{td: &taskDetail{Num: 99, Title: "other"}})
	if m.tv.Detail.Num == 99 {
		t.Error("a reload for a different task replaced the open one")
	}
	// ctrl+r reloads on demand.
	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
	runCmd(m, cmd)
	if rec.loads != base+2 {
		t.Errorf("ctrl+r should reload: %d", rec.loads)
	}
}
