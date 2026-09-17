// Package fleetsync decides what `fleet sync` should do, given the state of GitHub
// and of Multica. It is pure: no I/O, so every rule is table-tested.
//
// Ownership is one-directional so the two systems never fight:
//   - GitHub owns intake: which issues exist, their labels, dependencies, PRs, CI.
//   - Multica owns execution: runs, worktrees, sessions, its issue statuses.
//
// fleet decides only what is ELIGIBLE (ready labels, dependencies closed, not paused,
// attempts left). It never decides when or on which worker something runs; Multica's
// per-agent concurrency does that.
package fleetsync

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/noelzappy/fleet/internal/config"
)

// Metadata keys fleet sets on Multica issues. They are the only state fleet keeps,
// and they live in Multica, so fleet itself stays stateless between ticks.
const (
	MetaRepo     = "fleet_repo"         // owner/name this issue mirrors
	MetaKind     = "fleet_kind"         // task | review
	MetaIssue    = "gh_issue"           // GitHub issue number (task) or the PR's issue (review)
	MetaPR       = "gh_pr"              // PR number (review)
	MetaProfile  = "fleet_profile"      // profile the issue is assigned to
	MetaNudgedID = "gate_nudged_run"    // last gate run id the agent was told about
	MetaConflict = "conflict_nudged"    // head sha the agent was told conflicts with the base branch
	MetaAttrib   = "attribution_nudged" // head sha the agent was told carries an attribution trailer
	MetaBody     = "body_nudged"        // PR number the agent was told has a non-conforming body
	MetaRerun    = "rerun_of"           // id of the run fleet re-ran (server-cancelled or escalated failure)
	MetaFailEsc  = "failure_escalated"  // id of the failed run fleet escalated to GitHub
)

const (
	KindTask   = "task"
	KindReview = "review"

	StatusBlocked   = "blocked"
	StatusDone      = "done"
	StatusCancelled = "cancelled"
)

// GHIssue is what `gh issue list --json number,title,body,labels,state,url` gives us.
type GHIssue struct {
	Number int
	Title  string
	Body   string
	Labels []string
	State  string // OPEN | CLOSED
	URL    string
}

// MIssue is a Multica issue fleet created (metadata fleet_repo set).
type MIssue struct {
	ID         string
	Status     string
	Kind       string
	Issue      int    // gh_issue
	PR         int    // gh_pr, reviews only
	Profile    string // fleet_profile
	NudgedRun  string // gate_nudged_run
	Conflicted string // conflict_nudged
	Attributed string // attribution_nudged
	BodyNudged int    // body_nudged
	RerunOf    string // rerun_of
	FailureEsc string // failure_escalated
	// Latest run, filled for issues that should be working (todo, in_progress).
	LastRunID     string
	LastRunStatus string
	LastRunError  string
	RunActive     bool
	LastComment   string // newest comment body; filled only for blocked issues
}

// PR is an open pull request on a fleet branch (agent/<issue>-<slug>).
type PR struct {
	Number      int
	Head        string
	HeadSHA     string
	Issue       int // parsed from Head; 0 if not a fleet branch
	URL         string
	Conflicting bool     // gh mergeable == CONFLICTING
	Attribution []string // commit-message lines that attribute a commit to an agent or tool
	BodyErrors  []string // what the PR body is missing (Closes #N, Model: line)
	Gate        []GateRun
}

// GateRun is one CI run of the gate workflow on a PR's branch, newest first.
type GateRun struct {
	ID         string
	Conclusion string   // success | failure | ...
	FailedJobs []string // job names that failed
}

// State is everything a tick observed.
type State struct {
	GH      []GHIssue
	Multica []MIssue
	PRs     []PR
	// SignedOut holds harness names whose CLI isn't signed in; routing skips their profiles.
	SignedOut map[string]bool
}

// Action is one thing to do. Exactly one field group is set.
type Action struct {
	Kind string // create-task | unblock | escalate | stuck | nudge | rebase | create-review
	// create-task / unblock / escalate / stuck / nudge
	Issue   GHIssue
	Multica MIssue // existing Multica issue (unblock, escalate, stuck, nudge, create-review)
	Profile string // agent to assign / mention (create-task, nudge, create-review)
	Label   string // escalate: label to add; stuck: label to add
	Comment string // text to post (escalate → GitHub, nudge → Multica)
	PR      PR     // stuck, nudge, create-review
	Run     GateRun
}

func (a Action) String() string {
	switch a.Kind {
	case "create-task":
		return fmt.Sprintf("create-task    #%d → %s", a.Issue.Number, a.Profile)
	case "unblock":
		return fmt.Sprintf("unblock        #%d (%s)", a.Issue.Number, a.Multica.ID)
	case "escalate":
		return fmt.Sprintf("escalate       #%d +%s", a.Issue.Number, a.Label)
	case "stuck":
		return fmt.Sprintf("stuck          #%d PR #%d +%s", a.Issue.Number, a.PR.Number, a.Label)
	case "nudge":
		return fmt.Sprintf("nudge          #%d PR #%d run %s → @%s", a.Issue.Number, a.PR.Number, a.Run.ID, a.Profile)
	case "rebase":
		return fmt.Sprintf("rebase         #%d PR #%d %s → @%s", a.Issue.Number, a.PR.Number, a.PR.HeadSHA, a.Profile)
	case "rerun":
		return fmt.Sprintf("rerun          %s %s (run %s cancelled by server)", a.Multica.Kind, a.Multica.ID, a.Multica.LastRunID)
	case "escalate-failure":
		return fmt.Sprintf("escalate-fail  %s #%d run %s (%s) +%s", a.Multica.Kind, a.Issue.Number, a.Multica.LastRunID, a.Multica.Profile, a.Label)
	case "retry":
		if a.Profile != a.Multica.Profile {
			return fmt.Sprintf("retry          %s #%d %s → %s (re-routed)", a.Multica.Kind, a.Issue.Number, a.Multica.Profile, a.Profile)
		}
		return fmt.Sprintf("retry          %s #%d → %s", a.Multica.Kind, a.Issue.Number, a.Profile)
	case "close":
		return fmt.Sprintf("close          %s %s (%s)", a.Multica.Kind, a.Multica.ID, a.Comment)
	case "fix-body":
		return fmt.Sprintf("fix-body       #%d PR #%d → @%s", a.Issue.Number, a.PR.Number, a.Profile)
	case "strip-attribution":
		return fmt.Sprintf("strip-attrib   #%d PR #%d %s → @%s", a.Issue.Number, a.PR.Number, a.PR.HeadSHA, a.Profile)
	case "create-review":
		return fmt.Sprintf("create-review  PR #%d (#%d) → %s", a.PR.Number, a.Issue.Number, a.Profile)
	}
	return a.Kind
}

// Plan computes the actions for one tick. Actions are ordered by issue number so
// output is stable and a partial failure is easy to resume.
func Plan(f *config.Fleet, st State) []Action {
	L := f.Labels
	byNum := map[int]GHIssue{}
	paused := false
	for _, is := range st.GH {
		byNum[is.Number] = is
		if is.State == "OPEN" && has(is.Labels, L.Paused) {
			paused = true
		}
	}
	task := map[int]MIssue{}   // gh issue → task
	review := map[int]MIssue{} // gh pr → review
	for _, m := range st.Multica {
		switch m.Kind {
		case KindTask:
			// A cancelled task (fleet kill) no longer mirrors the issue: relabelling it
			// agent-ready dispatches a fresh one. A live task always wins over a cancelled one.
			if prev, ok := task[m.Issue]; m.Status == StatusCancelled || (ok && prev.Status != StatusCancelled) {
				continue
			}
			task[m.Issue] = m
		case KindReview:
			review[m.PR] = m
		}
	}

	var out []Action
	nums := make([]int, 0, len(byNum))
	for n := range byNum {
		nums = append(nums, n)
	}
	sort.Ints(nums)

	for _, n := range nums {
		is := byNum[n]
		if is.State != "OPEN" {
			continue
		}
		m, mirrored := task[n]
		if mirrored && m.Status == StatusBlocked && !hasAny(is.Labels, escalationLabels(L)) {
			label, comment := escalationFor(m.LastComment, L)
			out = append(out, Action{Kind: "escalate", Issue: is, Multica: m, Label: label, Comment: comment})
			continue // the owner's answer (label removed) unblocks it on a later tick
		}
		if paused || !Eligible(f, is, byNum) {
			continue
		}
		switch {
		case !mirrored:
			p, ok := routeImplementer(f, is, st.SignedOut)
			if ok {
				out = append(out, Action{Kind: "create-task", Issue: is, Profile: p})
			}
		case m.Status == StatusBlocked:
			out = append(out, Action{Kind: "unblock", Issue: is, Multica: m})
		}
	}

	// Closure flows back: a task whose GitHub issue closed, or a review whose PR is no
	// longer open, is done in Multica too, so its board and `fleet status` stay truthful.
	openPRs := map[int]bool{}
	for _, pr := range st.PRs {
		openPRs[pr.Number] = true
	}
	for _, m := range st.Multica {
		if m.Status == StatusDone || m.Status == StatusCancelled {
			continue
		}
		switch m.Kind {
		case KindTask:
			if is, ok := byNum[m.Issue]; ok && is.State == "CLOSED" {
				out = append(out, Action{Kind: "close", Issue: is, Multica: m, Comment: fmt.Sprintf("GitHub #%d closed", m.Issue)})
			}
		case KindReview:
			if m.PR != 0 && !openPRs[m.PR] {
				out = append(out, Action{Kind: "close", Issue: byNum[m.Issue], Multica: m, Comment: fmt.Sprintf("PR #%d no longer open", m.PR)})
			}
		}
	}

	// A run that failed on the agent's side (signed-out CLI, exhausted quota, crash) is
	// never retried silently: it's escalated to the GitHub issue with the error, once per
	// failed run. When the owner removes the label, the work is retried, re-routed to a
	// signed-in profile if the original one's harness is still signed out.
	for _, m := range st.Multica {
		if !AgentFailed(m) {
			continue
		}
		is, ok := byNum[m.Issue]
		if !ok || is.State != "OPEN" || has(is.Labels, L.Stuck) {
			continue
		}
		if m.Kind == KindTask && task[m.Issue].ID != m.ID {
			continue
		}
		if m.Kind == KindReview && !openPRs[m.PR] {
			continue
		}
		labelled := hasAny(is.Labels, escalationLabels(L))
		switch {
		case m.LastRunID != m.FailureEsc && !labelled:
			what := fmt.Sprintf("working on #%d", m.Issue)
			if m.Kind == KindReview {
				what = fmt.Sprintf("reviewing PR #%d", m.PR)
			}
			out = append(out, Action{Kind: "escalate-failure", Issue: is, Multica: m, Label: L.NeedsHuman,
				Comment: fmt.Sprintf("The %s agent failed while %s (harness %s, run %s):\n\n```\n%s\n```\n\nIf the harness is signed out, sign it in on the box (README › Signing in over SSH). Then remove the `%s` label: fleet retries, re-routing to a signed-in profile if this one still isn't.",
					m.Profile, what, f.Profiles[m.Profile].Harness, m.LastRunID, truncate(m.LastRunError, 600), L.NeedsHuman)})
		case m.LastRunID == m.FailureEsc && !labelled && m.RerunOf != m.LastRunID && !paused:
			p := m.Profile
			if st.SignedOut[f.Profiles[p].Harness] {
				var ok bool
				if m.Kind == KindTask {
					p, ok = routeImplementer(f, is, st.SignedOut)
				} else {
					p, ok = routeReviewer(f, task[m.Issue].Profile, m.PR, st.SignedOut)
				}
				if !ok {
					continue // nothing signed in can take it yet
				}
			}
			out = append(out, Action{Kind: "retry", Issue: is, Multica: m, Profile: p})
		}
	}

	// Runs stranded by a daemon restart: Multica marks them failed with "task cancelled
	// by server" and doesn't retry, leaving the issue in todo forever. This is the one
	// failure fleet re-runs — never agent errors, which stay the agent's to report.
	openPR := map[int]bool{}
	for _, pr := range st.PRs {
		openPR[pr.Number] = true
	}
	if !paused {
		for _, m := range st.Multica {
			if !Stranded(m) {
				continue
			}
			switch m.Kind {
			case KindTask:
				is, ok := byNum[m.Issue]
				if !ok || is.State != "OPEN" || has(is.Labels, L.Stuck) || task[m.Issue].ID != m.ID {
					continue
				}
				out = append(out, Action{Kind: "rerun", Issue: is, Multica: m})
			case KindReview:
				if openPR[m.PR] {
					out = append(out, Action{Kind: "rerun", Issue: byNum[m.Issue], Multica: m})
				}
			}
		}
	}

	prs := append([]PR(nil), st.PRs...)
	sort.Slice(prs, func(i, j int) bool { return prs[i].Number < prs[j].Number })
	for _, pr := range prs {
		m, mirrored := task[pr.Issue]
		is, open := byNum[pr.Issue]
		if pr.Issue == 0 || !mirrored || !open || is.State != "OPEN" || has(is.Labels, L.Stuck) {
			continue
		}
		// The PR body contract (Closes #N, Model: <profile>) is what pr-contract and the
		// reviewer key off; tell the implementer once per PR.
		if len(pr.BodyErrors) > 0 && pr.Number != m.BodyNudged {
			out = append(out, Action{Kind: "fix-body", Issue: is, Multica: m, PR: pr, Profile: m.Profile,
				Comment: fmt.Sprintf("@%s The body of PR %s doesn't follow the PR contract in AGENTS.md: %s. Edit the PR body (`gh pr edit %d --body-file -`) to include `Closes #%d`, the sections What / AC→tests / Scope check / Not done, and the line `Model: %s`.",
					m.Profile, pr.URL, strings.Join(pr.BodyErrors, "; "), pr.Number, is.Number, m.Profile)})
		}
		// Commits are the owner's: an attribution trailer blocks merge (pr-contract), so
		// tell the implementer once per head to amend it away.
		if len(pr.Attribution) > 0 && pr.HeadSHA != m.Attributed {
			out = append(out, Action{Kind: "strip-attribution", Issue: is, Multica: m, PR: pr, Profile: m.Profile,
				Comment: fmt.Sprintf("@%s Commits on PR %s carry attribution lines that are not allowed (commits are the owner's; see AGENTS.md):\n%s\nRemove them (`git commit --amend`, `git rebase -i` for older commits), make sure no hook re-adds them, and force-push.", m.Profile, pr.URL, "  "+strings.Join(pr.Attribution, "\n  "))})
		}
		// A conflicting PR can't merge and its gate is moot; tell the implementer once per head.
		if pr.Conflicting && pr.HeadSHA != m.Conflicted {
			out = append(out, Action{Kind: "rebase", Issue: is, Multica: m, PR: pr, Profile: m.Profile,
				Comment: fmt.Sprintf("@%s PR %s conflicts with `%s`. Rebase onto `origin/%s`, resolve the conflicts, run `%s`, and force-push the branch.", m.Profile, pr.URL, f.Project.Branch, f.Project.Branch, f.Gate.Command)})
		}
		if len(pr.Gate) > 0 && pr.Gate[0].Conclusion == "failure" {
			if failures(pr.Gate) >= f.Routing.MaxGateAttempts {
				out = append(out, Action{Kind: "stuck", Issue: is, Multica: m, PR: pr, Label: L.Stuck,
					Comment: fmt.Sprintf("Gate failed %d times on %s (max_gate_attempts=%d). Cancelling the session; split or clarify the issue and relabel %s.", failures(pr.Gate), pr.URL, f.Routing.MaxGateAttempts, L.Ready)})
				continue
			}
			if run := pr.Gate[0]; run.ID != m.NudgedRun {
				p := fixerFor(f, m.Profile, run, st.SignedOut)
				out = append(out, Action{Kind: "nudge", Issue: is, Multica: m, PR: pr, Run: run, Profile: p,
					Comment: fmt.Sprintf("@%s The gate failed on PR %s (attempt %d of %d). Failed jobs: %s. Fix it and push.", p, pr.URL, failures(pr.Gate), f.Routing.MaxGateAttempts, strings.Join(run.FailedJobs, ", "))})
			}
		}
		if _, reviewed := review[pr.Number]; !reviewed && !paused {
			if r, ok := routeReviewer(f, m.Profile, pr.Number, st.SignedOut); ok {
				out = append(out, Action{Kind: "create-review", Issue: is, Multica: m, PR: pr, Profile: r})
			}
		}
	}
	return out
}

// Eligible is the dispatch rule from the README: ready label, no blocking label,
// every dependency closed. Pause is checked by the caller.
func Eligible(f *config.Fleet, is GHIssue, all map[int]GHIssue) bool {
	L := f.Labels
	if !has(is.Labels, L.Ready) && !has(is.Labels, L.Assist) {
		return false
	}
	if hasAny(is.Labels, append(escalationLabels(L), L.BlockedBy, L.HumanRequired)) {
		return false
	}
	for _, dep := range DependsOn(is.Body) {
		d, ok := all[dep]
		if !ok || d.State != "CLOSED" {
			return false
		}
	}
	return true
}

var (
	dependsHeading = regexp.MustCompile(`(?im)^#+\s*depends on\b.*$|^depends on:`)
	nextHeading    = regexp.MustCompile(`(?m)^#+\s`)
	issueRef       = regexp.MustCompile(`#(\d+)`)
)

// DependsOn extracts #N references from the "Depends on" section of an issue body —
// the section `fleet issues sync` writes, or a "Depends on:" line.
func DependsOn(body string) []int {
	loc := dependsHeading.FindStringIndex(body)
	if loc == nil {
		return nil
	}
	section := body[loc[1]:]
	if end := nextHeading.FindStringIndex(section); end != nil {
		section = section[:end[0]]
	}
	var deps []int
	seen := map[int]bool{}
	for _, m := range issueRef.FindAllStringSubmatch(section, -1) {
		n, _ := strconv.Atoi(m[1])
		if !seen[n] {
			seen[n] = true
			deps = append(deps, n)
		}
	}
	return deps
}

// IssueFromBranch parses agent/<issue>-<slug> → issue, or 0.
func IssueFromBranch(head string) int {
	rest, ok := strings.CutPrefix(head, "agent/")
	if !ok {
		return 0
	}
	digits := rest
	if i := strings.IndexByte(rest, '-'); i >= 0 {
		digits = rest[:i]
	}
	n, _ := strconv.Atoi(digits)
	return n
}

// routeImplementer picks the implementer for the issue's wave. With several
// candidates it spreads by issue number, which is deterministic and stateless;
// it is not load-aware — Multica's per-agent concurrency queues the rest.
func routeImplementer(f *config.Fleet, is GHIssue, out map[string]bool) (string, bool) {
	wave := ""
	for _, l := range is.Labels {
		if w := f.WaveForLabel(l); w != "" {
			wave = w
			break
		}
	}
	if wave == "" {
		return "", false
	}
	c := f.ProfilesWhere(func(_ string, p config.Profile) bool {
		return p.Role == config.RoleImplementer && p.Concurrency > 0 && contains(p.Waves, wave) && !out[p.Harness]
	})
	if len(c) == 0 {
		return "", false
	}
	return c[is.Number%len(c)], true
}

// routeReviewer picks a reviewer whose vendor differs from the implementer's when
// cross_vendor_review is on. Spread by PR number.
func routeReviewer(f *config.Fleet, implementer string, pr int, out map[string]bool) (string, bool) {
	impl := f.Profiles[implementer]
	c := f.ProfilesWhere(func(_ string, p config.Profile) bool {
		return p.Role == config.RoleReviewer && p.Concurrency > 0 && (!f.Routing.CrossVendorReview || p.Vendor != impl.Vendor) && !out[p.Harness]
	})
	if len(c) == 0 {
		return "", false
	}
	return c[pr%len(c)], true
}

var lintJob = regexp.MustCompile(`(?i)lint|typecheck|type-check|format|prettier|eslint|tsc`)

// fixerFor returns the fixer when every failed job is lint/typecheck-shaped and
// routing.fixer_only_lint is set; otherwise the implementer keeps the failure.
func fixerFor(f *config.Fleet, implementer string, run GateRun, out map[string]bool) string {
	if !f.Routing.FixerOnlyLint || len(run.FailedJobs) == 0 {
		return implementer
	}
	for _, j := range run.FailedJobs {
		if !lintJob.MatchString(j) {
			return implementer
		}
	}
	c := f.ProfilesWhere(func(_ string, p config.Profile) bool {
		return p.Role == config.RoleFixer && p.Concurrency > 0 && !out[p.Harness]
	})
	if len(c) == 0 {
		return implementer
	}
	return c[0]
}

func escalationLabels(L config.Labels) []string {
	return []string{L.NeedsHuman, L.NeedsResource, L.NeedsContract, L.Stuck}
}

// escalationFor maps an agent's block comment to a label: the comment names the
// label it wants (the AGENTS.md protocol), else needs-human.
func escalationFor(comment string, L config.Labels) (label, body string) {
	label = L.NeedsHuman
	for _, l := range []string{L.NeedsResource, L.NeedsContract, L.NeedsHuman} {
		if strings.Contains(comment, l) {
			label = l
			break
		}
	}
	body = "Agent blocked in Multica."
	if strings.TrimSpace(comment) != "" {
		body += "\n\n" + strings.TrimSpace(comment)
	}
	body += "\n\nAnswer here, then remove the `" + label + "` label to resume."
	return label, body
}

func failures(runs []GateRun) int {
	n := 0
	for _, r := range runs {
		if r.Conclusion == "failure" {
			n++
		}
	}
	return n
}

func has(list []string, v string) bool { return contains(list, v) }

func hasAny(list, any []string) bool {
	for _, v := range any {
		if v != "" && contains(list, v) {
			return true
		}
	}
	return false
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

var attributionLine = regexp.MustCompile(`(?im)^(co-authored-by|signed-off-by):.*(agent|bot|claude|gemini|gpt|codex|opencode|multica|anthropic|google|openai)|generated with|claude-session`)

// AttributionLines returns the lines of a commit message that attribute it to an
// agent, bot, model or tool. Mirrors the pr-contract check.
func AttributionLines(message string) []string {
	var out []string
	for _, line := range strings.Split(message, "\n") {
		if attributionLine.MatchString(line) {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return out
}

var (
	closesRef = regexp.MustCompile(`(?i)\bcloses #(\d+)`)
	modelLine = regexp.MustCompile(`(?m)^Model: *([A-Za-z0-9_-]+)`)
)

// BodyErrors checks a PR body against the contract: it must close its issue and
// name the implementing profile. Mirrors the pr-contract check.
func BodyErrors(body string, issue int, profile string) []string {
	var errs []string
	ok := false
	for _, m := range closesRef.FindAllStringSubmatch(body, -1) {
		if n, _ := strconv.Atoi(m[1]); n == issue {
			ok = true
		}
	}
	if !ok {
		errs = append(errs, fmt.Sprintf("missing `Closes #%d`", issue))
	}
	if m := modelLine.FindStringSubmatch(body); m == nil {
		errs = append(errs, "missing `Model: <profile>` line")
	} else if profile != "" && m[1] != profile {
		errs = append(errs, fmt.Sprintf("`Model: %s` should be `Model: %s`", m[1], profile))
	}
	return errs
}

// Stranded reports a Multica issue that should be working but whose latest run was
// cancelled by the server (a daemon restart) and that fleet hasn't re-run yet.
func Stranded(m MIssue) bool {
	return (m.Status == "todo" || m.Status == "in_progress") && !m.RunActive &&
		m.LastRunStatus == "failed" && strings.Contains(m.LastRunError, "cancelled by server") &&
		m.LastRunID != "" && m.LastRunID != m.RerunOf
}

// AgentFailed reports a Multica issue that should be working whose newest run failed on
// the agent's side (anything but a server cancellation), with nothing running now.
func AgentFailed(m MIssue) bool {
	return (m.Status == "todo" || m.Status == "in_progress") && !m.RunActive &&
		m.LastRunStatus == "failed" && m.LastRunID != "" && !strings.Contains(m.LastRunError, "cancelled by server")
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
