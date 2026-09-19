package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/noelzappy/fleet/internal/fleetsync"
	"github.com/noelzappy/fleet/internal/shell"
)

// The task view answers "what is going on with #7?" and lets the owner talk to the
// agent. Everything it shows is gathered here; nothing is stored, so closing the view and
// reopening it always shows the current Multica, GitHub and worktree state.

type runInfo struct {
	ID      string
	Status  string
	Error   string
	Attempt int
	Started time.Time
	Ended   time.Time
	WorkDir string
}

type commentInfo struct {
	Author  string // agent | member
	Content string
	At      time.Time
}

// worktreeInfo is what the agent has left on disk: enough to tell "wrote code, never
// committed" from "nothing happened", without sending the code itself anywhere.
type worktreeInfo struct {
	Path      string
	Branch    string
	Changed   []string // git status --short lines, capped
	More      int      // changed files beyond the cap
	Shortstat string   // "4 files changed, 262 insertions(+), 1 deletion(-)"
	Log       []string // last commits
}

type taskDetail struct {
	Num      int
	Title    string
	URL      string
	Body     string
	Row      issueRow
	Task     fleetsync.MIssue
	HasTask  bool
	Runs     []runInfo // newest first
	Comments []commentInfo
	PR       *prRow
	Work     worktreeInfo
	LoadedAt time.Time
	Notes    []string // parts that couldn't be read; the rest still shows
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

func parseRuns(out string) []runInfo {
	rows, _, err := jsonRows(out, "runs", "tasks")
	if err != nil {
		return nil
	}
	var runs []runInfo
	for _, r := range rows {
		runs = append(runs, runInfo{ID: str(r["id"]), Status: str(r["status"]), Error: str(r["error"]), Attempt: num(r["attempt"]),
			Started: parseTime(str(r["started_at"])), Ended: parseTime(str(r["completed_at"])), WorkDir: str(r["work_dir"])})
	}
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].Started.After(runs[j].Started) })
	return runs
}

func parseComments(out string) []commentInfo {
	rows, _, err := jsonRows(out, "comments")
	if err != nil {
		return nil
	}
	var cs []commentInfo
	for _, r := range rows {
		author := str(r["author_type"])
		if author == "" {
			author = "?"
		}
		cs = append(cs, commentInfo{Author: author, Content: strings.TrimSpace(str(r["content"])), At: parseTime(str(r["created_at"]))})
	}
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].At.Before(cs[j].At) })
	return cs
}

// findGitDir finds the checkout inside a run's work_dir. Multica lays it out as
// <work_dir>/workdir/<repo>, but a run's recorded dir may be the parent or the checkout
// itself, so look a couple of levels down.
func findGitDir(root string) string {
	var walk func(dir string, depth int) string
	walk = func(dir string, depth int) string {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		if depth == 0 {
			return ""
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return ""
		}
		for _, e := range entries {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") && e.Name() != "node_modules" {
				if found := walk(filepath.Join(dir, e.Name()), depth-1); found != "" {
					return found
				}
			}
		}
		return ""
	}
	return walk(root, 2)
}

const maxChangedShown = 12

func loadWorktree(ctx context.Context, workDir string) worktreeInfo {
	dir := findGitDir(workDir)
	if dir == "" {
		return worktreeInfo{}
	}
	git := func(args string) string {
		out, _ := shell.Output(ctx, "git -C "+shell.Quote(dir)+" "+args+" 2>/dev/null")
		return out
	}
	w := worktreeInfo{Path: dir, Branch: git("branch --show-current"), Shortstat: git("diff --shortstat HEAD")}
	for _, l := range strings.Split(git("status --short"), "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if len(w.Changed) < maxChangedShown {
			w.Changed = append(w.Changed, l)
		} else {
			w.More++
		}
	}
	for _, l := range strings.Split(git("log --oneline -3"), "\n") {
		if strings.TrimSpace(l) != "" {
			w.Log = append(w.Log, l)
		}
	}
	return w
}

// baseDetail is the part of a task view the snapshot already has: no calls, so the view
// can open at once and fill in the rest.
func baseDetail(s *snapshot, n int) (*taskDetail, error) {
	td := &taskDetail{Num: n, LoadedAt: time.Now()}
	found := false
	for _, is := range s.State.GH {
		if is.Number == n {
			td.Title, td.URL, td.Body, found = is.Title, is.URL, is.Body, true
		}
	}
	if !found {
		return nil, fmt.Errorf("issue #%d isn't in the current snapshot", n)
	}
	for _, r := range s.Issues {
		if r.Num == n {
			td.Row = r
		}
	}
	for _, p := range s.PRs {
		if p.Issue == n {
			pr := p
			td.PR = &pr
		}
	}
	for _, m := range s.State.Multica {
		if m.Kind == fleetsync.KindTask && m.Issue == n && m.Status != fleetsync.StatusCancelled {
			td.Task, td.HasTask = m, true
		}
	}
	return td, nil
}

// loadTaskDetail gathers one issue's task view from the snapshot plus the parts that need
// their own calls (runs, comments, the worktree). A failing part is noted, not fatal.
func loadTaskDetail(ctx context.Context, s *snapshot, n int) (*taskDetail, error) {
	td, err := baseDetail(s, n)
	if err != nil || !td.HasTask {
		return td, err
	}
	if out, err := shell.Output(ctx, multica+" issue runs "+shell.Quote(td.Task.ID)+" --output json"); err != nil {
		td.Notes = append(td.Notes, "couldn't read runs: "+err.Error())
	} else {
		td.Runs = parseRuns(out)
	}
	if out, err := shell.Output(ctx, multica+" issue comment list "+shell.Quote(td.Task.ID)+" --output json --compact --recent 8"); err != nil {
		td.Notes = append(td.Notes, "couldn't read comments: "+err.Error())
	} else {
		td.Comments = parseComments(out)
		if len(td.Comments) > 12 {
			td.Comments = td.Comments[len(td.Comments)-12:]
		}
	}
	if len(td.Runs) > 0 && td.Runs[0].WorkDir != "" {
		td.Work = loadWorktree(ctx, td.Runs[0].WorkDir)
	}
	return td, nil
}

// exchange is one line of the conversation in the task view.
type exKind int

const (
	exAsk    exKind = iota // the owner's question
	exAnswer               // the model's answer
	exTell                 // a follow-up sent to the agent
	exNote                 // fleet telling the owner something (sent, failed, can't)
)

type exchange struct {
	Kind exKind
	Text string
	At   time.Time
	Bad  bool
}

const (
	maxPromptBody     = 3000
	maxPromptComment  = 1200
	maxPromptThread   = 6
	askInstructions   = "You answer an engineering manager's question about ONE task in a fleet of coding agents. Use only the context below. If it doesn't contain the answer, say what is missing and what to look at; do not guess. Be concrete and short: a few sentences, or a short list. You have no tools and cannot change anything."
	noChangesInWorktr = "nothing (no checkout found for the latest run)"
)

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…[cut]"
}

// askPrompt is the entire context a question is answered from. It is plain text built from
// what the task view already shows, so the answer can't rely on anything the owner can't see.
func askPrompt(td *taskDetail, thread []exchange, question string, now time.Time) string {
	var b strings.Builder
	b.WriteString(askInstructions + "\n\n")
	fmt.Fprintf(&b, "TASK: GitHub issue #%d %q\nFleet's view of it: %s", td.Num, td.Title, td.Row.State)
	if td.Row.Why != "" {
		fmt.Fprintf(&b, " (%s)", td.Row.Why)
	}
	b.WriteString("\n")
	if td.HasTask {
		fmt.Fprintf(&b, "Agent profile: %s. Multica status: %s.\n", td.Task.Profile, td.Task.Status)
	} else {
		b.WriteString("No agent has been assigned yet (it has not been dispatched).\n")
	}
	if td.Body != "" {
		fmt.Fprintf(&b, "\nISSUE DESCRIPTION:\n%s\n", clip(td.Body, maxPromptBody))
	}
	if len(td.Runs) > 0 {
		b.WriteString("\nRUNS (newest first):\n")
		for _, r := range td.Runs {
			fmt.Fprintf(&b, "- %s, started %s", r.Status, r.Started.Local().Format("15:04"))
			if !r.Ended.IsZero() {
				fmt.Fprintf(&b, ", ended %s (%s)", r.Ended.Local().Format("15:04"), r.Ended.Sub(r.Started).Round(time.Second))
			}
			if r.Error != "" {
				fmt.Fprintf(&b, ", error: %s", clip(r.Error, 300))
			}
			b.WriteString("\n")
		}
	}
	if len(td.Comments) > 0 {
		b.WriteString("\nCOMMENTS ON THE TASK (oldest first):\n")
		for _, c := range td.Comments {
			who := c.Author
			if who == "member" {
				who = "owner"
			}
			fmt.Fprintf(&b, "[%s %s] %s\n", who, c.At.Local().Format("15:04"), clip(c.Content, maxPromptComment))
		}
	}
	if td.PR != nil {
		fmt.Fprintf(&b, "\nPULL REQUEST #%d (%s): gate %s", td.PR.Num, td.PR.Head, orDash(td.PR.Gate))
		if len(td.PR.Flags) > 0 {
			fmt.Fprintf(&b, "; %s", strings.Join(td.PR.Flags, "; "))
		}
		b.WriteString("\n")
	} else {
		b.WriteString("\nNo pull request yet.\n")
	}
	w := td.Work
	if w.Path == "" {
		fmt.Fprintf(&b, "\nAGENT'S WORKTREE: %s\n", noChangesInWorktr)
	} else {
		fmt.Fprintf(&b, "\nAGENT'S WORKTREE: branch %s", orDash(w.Branch))
		if w.Shortstat != "" {
			fmt.Fprintf(&b, ", %s", w.Shortstat)
		}
		b.WriteString("\n")
		if len(w.Changed) == 0 {
			b.WriteString("Uncommitted changes: none.\n")
		} else {
			b.WriteString("Uncommitted files (git status):\n")
			for _, l := range w.Changed {
				b.WriteString("  " + l + "\n")
			}
			if w.More > 0 {
				fmt.Fprintf(&b, "  …and %d more\n", w.More)
			}
		}
		if len(w.Log) > 0 {
			b.WriteString("Recent commits: " + strings.Join(w.Log, " | ") + "\n")
		}
	}
	// Earlier questions in this session, so "why?" and "and the second one?" make sense.
	hist := thread
	if len(hist) > maxPromptThread {
		hist = hist[len(hist)-maxPromptThread:]
	}
	var earlier []string
	for _, e := range hist {
		switch e.Kind {
		case exAsk:
			earlier = append(earlier, "Owner asked: "+clip(e.Text, 400))
		case exAnswer:
			if !e.Bad {
				earlier = append(earlier, "You answered: "+clip(e.Text, 600))
			}
		case exTell:
			earlier = append(earlier, "Owner told the agent: "+clip(e.Text, 400))
		}
	}
	if len(earlier) > 0 {
		b.WriteString("\nEARLIER IN THIS CONVERSATION:\n" + strings.Join(earlier, "\n") + "\n")
	}
	fmt.Fprintf(&b, "\nNOW (%s). QUESTION: %s\n", now.Local().Format("15:04 Mon"), strings.TrimSpace(question))
	return b.String()
}

// pickAskHarness chooses which signed-in CLI answers: claude-code first, then codex, the
// ones with a way to switch their tools off.
func pickAskHarness(signedOut map[string]bool) (name string, build func(string) string, err error) {
	names := sortedHarnessNames()
	for _, kind := range []string{"claude-code", "codex"} {
		for _, n := range names {
			h := cfg.Harnesses[n]
			if h.Kind == kind && !signedOut[n] && harnessKinds[kind].ask != nil {
				return n, harnessKinds[kind].ask, nil
			}
		}
	}
	return "", nil, fmt.Errorf("no signed-in claude-code or codex harness to answer with")
}

// askModel runs the question through a tool-less headless CLI. It runs from a scratch
// directory so the CLI can't pick up the project's files or instructions.
func askModel(ctx context.Context, signedOut map[string]bool, prompt string) (answer, harness string, err error) {
	name, build, err := pickAskHarness(signedOut)
	if err != nil {
		return "", "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	out, err := shell.Output(ctx, "cd "+shell.Quote(os.TempDir())+" && "+build(prompt))
	if err != nil {
		return "", name, fmt.Errorf("%s couldn't answer: %w", name, err)
	}
	if strings.TrimSpace(out) == "" {
		return "", name, fmt.Errorf("%s returned an empty answer", name)
	}
	return strings.TrimSpace(out), name, nil
}

// canTell reports whether a follow-up can reach an agent, and why not otherwise.
func canTell(td *taskDetail) (bool, string) {
	switch {
	case td == nil || !td.HasTask:
		return false, fmt.Sprintf("#%d hasn't been dispatched, so there's no agent to tell yet", td.Num)
	case td.Task.Status == fleetsync.StatusDone:
		return false, fmt.Sprintf("#%d is done", td.Num)
	}
	return true, ""
}

// followUpText is what the agent reads: the owner's words, marked as such, mentioning the
// agent so the comment wakes it.
func followUpText(profile, text string) string {
	return fmt.Sprintf("@%s Follow-up from the owner (sent from fleet watch):\n\n%s", profile, strings.TrimSpace(text))
}

// sendFollowUp comments on the task in Multica, which wakes its agent. A task the agent
// had blocked on is unblocked too, as `fleet sync` does when you answer on GitHub.
func sendFollowUp(ctx context.Context, td *taskDetail, text string) error {
	if ok, why := canTell(td); !ok {
		return fmt.Errorf("%s", why)
	}
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("nothing to send")
	}
	if err := multicaComment(ctx, td.Task.ID, followUpText(td.Task.Profile, text)); err != nil {
		return fmt.Errorf("multica comment: %w", err)
	}
	if td.Task.Status == fleetsync.StatusBlocked {
		if err := shell.Run(ctx, multica+" issue status "+shell.Quote(td.Task.ID)+" todo >/dev/null", nil); err != nil {
			return fmt.Errorf("sent, but couldn't unblock the task: %w", err)
		}
	}
	return nil
}
