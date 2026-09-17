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
	MetaRepo     = "fleet_repo"      // owner/name this issue mirrors
	MetaKind     = "fleet_kind"      // task | review
	MetaIssue    = "gh_issue"        // GitHub issue number (task) or the PR's issue (review)
	MetaPR       = "gh_pr"           // PR number (review)
	MetaProfile  = "fleet_profile"   // profile the issue is assigned to
	MetaNudgedID = "gate_nudged_run" // last gate run id the agent was told about
	MetaConflict = "conflict_nudged" // head sha the agent was told conflicts with the base branch
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
	ID          string
	Status      string
	Kind        string
	Issue       int    // gh_issue
	PR          int    // gh_pr, reviews only
	Profile     string // fleet_profile
	NudgedRun   string // gate_nudged_run
	Conflicted  string // conflict_nudged
	LastComment string // newest comment body; filled only for blocked issues
}

// PR is an open pull request on a fleet branch (agent/<issue>-<slug>).
type PR struct {
	Number      int
	Head        string
	HeadSHA     string
	Issue       int // parsed from Head; 0 if not a fleet branch
	URL         string
	Conflicting bool // gh mergeable == CONFLICTING
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
			p, ok := routeImplementer(f, is)
			if ok {
				out = append(out, Action{Kind: "create-task", Issue: is, Profile: p})
			}
		case m.Status == StatusBlocked:
			out = append(out, Action{Kind: "unblock", Issue: is, Multica: m})
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
				p := fixerFor(f, m.Profile, run)
				out = append(out, Action{Kind: "nudge", Issue: is, Multica: m, PR: pr, Run: run, Profile: p,
					Comment: fmt.Sprintf("@%s The gate failed on PR %s (attempt %d of %d). Failed jobs: %s. Fix it and push.", p, pr.URL, failures(pr.Gate), f.Routing.MaxGateAttempts, strings.Join(run.FailedJobs, ", "))})
			}
		}
		if _, reviewed := review[pr.Number]; !reviewed && !paused {
			if r, ok := routeReviewer(f, m.Profile, pr.Number); ok {
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
func routeImplementer(f *config.Fleet, is GHIssue) (string, bool) {
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
		return p.Role == config.RoleImplementer && p.Concurrency > 0 && contains(p.Waves, wave)
	})
	if len(c) == 0 {
		return "", false
	}
	return c[is.Number%len(c)], true
}

// routeReviewer picks a reviewer whose vendor differs from the implementer's when
// cross_vendor_review is on. Spread by PR number.
func routeReviewer(f *config.Fleet, implementer string, pr int) (string, bool) {
	impl := f.Profiles[implementer]
	c := f.ProfilesWhere(func(_ string, p config.Profile) bool {
		return p.Role == config.RoleReviewer && p.Concurrency > 0 && (!f.Routing.CrossVendorReview || p.Vendor != impl.Vendor)
	})
	if len(c) == 0 {
		return "", false
	}
	return c[pr%len(c)], true
}

var lintJob = regexp.MustCompile(`(?i)lint|typecheck|type-check|format|prettier|eslint|tsc`)

// fixerFor returns the fixer when every failed job is lint/typecheck-shaped and
// routing.fixer_only_lint is set; otherwise the implementer keeps the failure.
func fixerFor(f *config.Fleet, implementer string, run GateRun) string {
	if !f.Routing.FixerOnlyLint || len(run.FailedJobs) == 0 {
		return implementer
	}
	for _, j := range run.FailedJobs {
		if !lintJob.MatchString(j) {
			return implementer
		}
	}
	c := f.ProfilesWhere(func(_ string, p config.Profile) bool { return p.Role == config.RoleFixer && p.Concurrency > 0 })
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
