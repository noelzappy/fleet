package fleetsync

import (
	"reflect"
	"strings"
	"testing"

	"github.com/noelzappy/fleet/internal/config"
)

func fleet() *config.Fleet {
	f := &config.Fleet{
		Project:   config.Project{Name: "p", Repo: "o/p"},
		Harnesses: map[string]config.Harness{"cc": {Kind: "claude-code"}, "agy": {Kind: "antigravity"}},
		Profiles: map[string]config.Profile{
			"impl-a":   {Harness: "cc", Role: "implementer", Vendor: "anthropic", Concurrency: 2, Waves: []string{"backend"}},
			"impl-g":   {Harness: "agy", Role: "implementer", Vendor: "google", Concurrency: 2, Waves: []string{"ui", "backend"}},
			"impl-off": {Harness: "agy", Role: "implementer", Vendor: "google", Concurrency: 0, Waves: []string{"ui"}},
			"rev-a":    {Harness: "cc", Role: "reviewer", Vendor: "anthropic", Concurrency: 1},
			"rev-g":    {Harness: "agy", Role: "reviewer", Vendor: "google", Concurrency: 1},
			"fix-g":    {Harness: "agy", Role: "fixer", Vendor: "google", Concurrency: 1},
		},
		Waves:   []config.Wave{{Name: "ui"}, {Name: "backend"}},
		Routing: config.Routing{CrossVendorReview: true, MaxGateAttempts: 3, FixerOnlyLint: true},
		Gate:    config.Gate{Command: "pnpm gate"},
	}
	f.ApplyDefaults()
	return f
}

func open(n int, labels ...string) GHIssue {
	return GHIssue{Number: n, Title: "t", State: "OPEN", Labels: labels, URL: "u"}
}

func kinds(actions []Action) []string {
	var out []string
	for _, a := range actions {
		out = append(out, a.String())
	}
	return out
}

func TestPlan(t *testing.T) {
	f := fleet()
	tests := []struct {
		name string
		st   State
		want []string
	}{
		{"ready ui issue is created on the ui implementer",
			State{GH: []GHIssue{open(1, "agent-ready", "wave:ui")}},
			[]string{"create-task    #1 → impl-g"}},
		{"backend spreads across implementers by number",
			State{GH: []GHIssue{open(2, "agent-ready", "wave:backend"), open(3, "agent-ready", "wave:backend")}},
			[]string{"create-task    #2 → impl-a", "create-task    #3 → impl-g"}},
		{"concurrency 0 profile is skipped", // impl-off never chosen for ui
			State{GH: []GHIssue{open(4, "agent-ready", "wave:ui")}},
			[]string{"create-task    #4 → impl-g"}},
		{"no wave label: nothing",
			State{GH: []GHIssue{open(1, "agent-ready")}}, nil},
		{"needs-human blocks",
			State{GH: []GHIssue{open(1, "agent-ready", "wave:ui", "needs-human")}}, nil},
		{"human-required never dispatches",
			State{GH: []GHIssue{open(1, "agent-ready", "wave:ui", "human-required")}}, nil},
		{"open dependency blocks",
			State{GH: []GHIssue{open(1, "agent-ready", "wave:ui"), {Number: 2, State: "OPEN", Labels: []string{"agent-ready", "wave:ui"}, Body: "x\n\n## Depends on\n- #1\n"}}},
			[]string{"create-task    #1 → impl-g"}},
		{"closed dependency releases",
			State{GH: []GHIssue{{Number: 1, State: "CLOSED"}, {Number: 2, State: "OPEN", Labels: []string{"agent-ready", "wave:ui"}, Body: "## Depends on\n#1"}}},
			[]string{"create-task    #2 → impl-g"}},
		{"unknown dependency blocks",
			State{GH: []GHIssue{{Number: 2, State: "OPEN", Labels: []string{"agent-ready", "wave:ui"}, Body: "## Depends on\n#99"}}}, nil},
		{"paused: no new tasks",
			State{GH: []GHIssue{open(1, "agent-ready", "wave:ui"), open(50, "fleet-paused")}}, nil},
		{"cancelled task (kill) then relabelled: re-dispatch",
			State{GH: []GHIssue{open(1, "agent-ready", "wave:ui")}, Multica: []MIssue{{ID: "m1", Kind: "task", Issue: 1, Status: "cancelled"}}},
			[]string{"create-task    #1 → impl-g"}},
		{"cancelled and live task: live one counts",
			State{GH: []GHIssue{open(1, "agent-ready", "wave:ui")}, Multica: []MIssue{{ID: "m1", Kind: "task", Issue: 1, Status: "cancelled"}, {ID: "m2", Kind: "task", Issue: 1, Status: "in_progress"}}}, nil},
		{"task stranded by daemon restart: rerun",
			State{GH: []GHIssue{open(1, "agent-ready", "wave:ui")}, Multica: []MIssue{{ID: "m1", Kind: "task", Issue: 1, Status: "todo", LastRunID: "r1", LastRunStatus: "failed", LastRunError: "task cancelled by server"}}},
			[]string{"rerun          task m1 (run r1 cancelled by server)"}},
		{"review stranded, PR open: rerun",
			State{GH: []GHIssue{open(1, "agent-ready", "wave:ui")},
				Multica: []MIssue{{ID: "m1", Kind: "task", Issue: 1, Status: "in_review", Profile: "impl-g"}, {ID: "m2", Kind: "review", Issue: 1, PR: 10, Status: "todo", LastRunID: "r9", LastRunStatus: "failed", LastRunError: "task cancelled by server"}},
				PRs:     []PR{{Number: 10, Head: "agent/1-x", Issue: 1}}},
			[]string{"rerun          review m2 (run r9 cancelled by server)"}},
		{"already re-ran that run: nothing",
			State{GH: []GHIssue{open(1, "agent-ready", "wave:ui")}, Multica: []MIssue{{ID: "m1", Kind: "task", Issue: 1, Status: "todo", LastRunID: "r1", LastRunStatus: "failed", LastRunError: "task cancelled by server", RerunOf: "r1"}}}, nil},
		{"agent error is not re-run",
			State{GH: []GHIssue{open(1, "agent-ready", "wave:ui")}, Multica: []MIssue{{ID: "m1", Kind: "task", Issue: 1, Status: "todo", LastRunID: "r1", LastRunStatus: "failed", LastRunError: "build failed"}}}, nil},
		{"stranded but paused: wait",
			State{GH: []GHIssue{open(1, "agent-ready", "wave:ui"), open(9, "fleet-paused")}, Multica: []MIssue{{ID: "m1", Kind: "task", Issue: 1, Status: "todo", LastRunID: "r1", LastRunStatus: "failed", LastRunError: "task cancelled by server"}}}, nil},
		{"already mirrored: nothing",
			State{GH: []GHIssue{open(1, "agent-ready", "wave:ui")}, Multica: []MIssue{{ID: "m1", Kind: "task", Issue: 1, Status: "in_progress"}}}, nil},
		{"blocked in multica escalates once",
			State{GH: []GHIssue{open(1, "agent-ready", "wave:ui")}, Multica: []MIssue{{ID: "m1", Kind: "task", Issue: 1, Status: "blocked", LastComment: "Need the fixture: needs-resource"}}},
			[]string{"escalate       #1 +needs-resource"}},
		{"escalated and still labelled: wait",
			State{GH: []GHIssue{open(1, "agent-ready", "wave:ui", "needs-human")}, Multica: []MIssue{{ID: "m1", Kind: "task", Issue: 1, Status: "blocked"}}}, nil},
		{"label removed: unblock",
			State{GH: []GHIssue{open(1, "agent-ready", "wave:ui")}, Multica: []MIssue{{ID: "m1", Kind: "task", Issue: 1, Status: "blocked", LastComment: "needs-human: which?"}}},
			[]string{"escalate       #1 +needs-human"}}, // first tick escalates; unblock only after a label round-trip
		{"paused does not unblock",
			State{GH: []GHIssue{open(1, "agent-ready", "wave:ui"), open(9, "fleet-paused")}, Multica: []MIssue{{ID: "m1", Kind: "task", Issue: 1, Status: "in_progress"}}}, nil},
		{"open PR gets a cross-vendor review",
			State{GH: []GHIssue{open(1, "agent-ready", "wave:ui")},
				Multica: []MIssue{{ID: "m1", Kind: "task", Issue: 1, Status: "in_review", Profile: "impl-g"}},
				PRs:     []PR{{Number: 10, Head: "agent/1-x", Issue: 1}}},
			[]string{"create-review  PR #10 (#1) → rev-a"}},
		{"review already exists: nothing",
			State{GH: []GHIssue{open(1, "agent-ready", "wave:ui")},
				Multica: []MIssue{{ID: "m1", Kind: "task", Issue: 1, Status: "in_review", Profile: "impl-g"}, {ID: "m2", Kind: "review", Issue: 1, PR: 10}},
				PRs:     []PR{{Number: 10, Head: "agent/1-x", Issue: 1}}}, nil},
		{"gate failure nudges implementer once",
			State{GH: []GHIssue{open(1, "agent-ready", "wave:ui")},
				Multica: []MIssue{{ID: "m1", Kind: "task", Issue: 1, Status: "in_review", Profile: "impl-g"}, {ID: "m2", Kind: "review", Issue: 1, PR: 10}},
				PRs:     []PR{{Number: 10, Head: "agent/1-x", Issue: 1, Gate: []GateRun{{ID: "r1", Conclusion: "failure", FailedJobs: []string{"test"}}}}}},
			[]string{"nudge          #1 PR #10 run r1 → @impl-g"}},
		{"already nudged for this run: nothing",
			State{GH: []GHIssue{open(1, "agent-ready", "wave:ui")},
				Multica: []MIssue{{ID: "m1", Kind: "task", Issue: 1, Profile: "impl-g", NudgedRun: "r1"}, {ID: "m2", Kind: "review", Issue: 1, PR: 10}},
				PRs:     []PR{{Number: 10, Head: "agent/1-x", Issue: 1, Gate: []GateRun{{ID: "r1", Conclusion: "failure", FailedJobs: []string{"test"}}}}}}, nil},
		{"lint-only failure goes to the fixer",
			State{GH: []GHIssue{open(1, "agent-ready", "wave:ui")},
				Multica: []MIssue{{ID: "m1", Kind: "task", Issue: 1, Profile: "impl-g"}, {ID: "m2", Kind: "review", Issue: 1, PR: 10}},
				PRs:     []PR{{Number: 10, Head: "agent/1-x", Issue: 1, Gate: []GateRun{{ID: "r2", Conclusion: "failure", FailedJobs: []string{"lint", "typecheck"}}}}}},
			[]string{"nudge          #1 PR #10 run r2 → @fix-g"}},
		{"third failure: stuck",
			State{GH: []GHIssue{open(1, "agent-ready", "wave:ui")},
				Multica: []MIssue{{ID: "m1", Kind: "task", Issue: 1, Profile: "impl-g"}, {ID: "m2", Kind: "review", Issue: 1, PR: 10}},
				PRs: []PR{{Number: 10, Head: "agent/1-x", Issue: 1, Gate: []GateRun{
					{ID: "r3", Conclusion: "failure"}, {ID: "r2", Conclusion: "failure"}, {ID: "r1", Conclusion: "failure"}}}}},
			[]string{"stuck          #1 PR #10 +agent-stuck"}},
		{"gate passing: nothing to nudge",
			State{GH: []GHIssue{open(1, "agent-ready", "wave:ui")},
				Multica: []MIssue{{ID: "m1", Kind: "task", Issue: 1, Profile: "impl-g"}, {ID: "m2", Kind: "review", Issue: 1, PR: 10}},
				PRs:     []PR{{Number: 10, Head: "agent/1-x", Issue: 1, Gate: []GateRun{{ID: "r2", Conclusion: "success"}, {ID: "r1", Conclusion: "failure"}}}}}, nil},
		{"non-conforming PR body gets one fix nudge",
			State{GH: []GHIssue{open(1, "agent-ready", "wave:ui")},
				Multica: []MIssue{{ID: "m1", Kind: "task", Issue: 1, Profile: "impl-g"}, {ID: "m2", Kind: "review", Issue: 1, PR: 10}},
				PRs:     []PR{{Number: 10, Head: "agent/1-x", Issue: 1, HeadSHA: "abc", BodyErrors: []string{"missing `Closes #1`"}}}},
			[]string{"fix-body       #1 PR #10 → @impl-g"}},
		{"body already reported: nothing",
			State{GH: []GHIssue{open(1, "agent-ready", "wave:ui")},
				Multica: []MIssue{{ID: "m1", Kind: "task", Issue: 1, Profile: "impl-g", BodyNudged: 10}, {ID: "m2", Kind: "review", Issue: 1, PR: 10}},
				PRs:     []PR{{Number: 10, Head: "agent/1-x", Issue: 1, HeadSHA: "abc", BodyErrors: []string{"x"}}}}, nil},
		{"attribution trailer gets one strip nudge",
			State{GH: []GHIssue{open(1, "agent-ready", "wave:ui")},
				Multica: []MIssue{{ID: "m1", Kind: "task", Issue: 1, Profile: "impl-g"}, {ID: "m2", Kind: "review", Issue: 1, PR: 10}},
				PRs:     []PR{{Number: 10, Head: "agent/1-x", Issue: 1, HeadSHA: "abc", Attribution: []string{"Co-authored-by: multica-agent <github@multica.ai>"}}}},
			[]string{"strip-attrib   #1 PR #10 abc → @impl-g"}},
		{"attribution already reported for this head: nothing",
			State{GH: []GHIssue{open(1, "agent-ready", "wave:ui")},
				Multica: []MIssue{{ID: "m1", Kind: "task", Issue: 1, Profile: "impl-g", Attributed: "abc"}, {ID: "m2", Kind: "review", Issue: 1, PR: 10}},
				PRs:     []PR{{Number: 10, Head: "agent/1-x", Issue: 1, HeadSHA: "abc", Attribution: []string{"Co-authored-by: x-agent <a@b>"}}}}, nil},
		{"conflicting PR gets one rebase nudge",
			State{GH: []GHIssue{open(1, "agent-ready", "wave:ui")},
				Multica: []MIssue{{ID: "m1", Kind: "task", Issue: 1, Profile: "impl-g"}, {ID: "m2", Kind: "review", Issue: 1, PR: 10}},
				PRs:     []PR{{Number: 10, Head: "agent/1-x", Issue: 1, HeadSHA: "abc", Conflicting: true}}},
			[]string{"rebase         #1 PR #10 abc → @impl-g"}},
		{"already told about this head: nothing",
			State{GH: []GHIssue{open(1, "agent-ready", "wave:ui")},
				Multica: []MIssue{{ID: "m1", Kind: "task", Issue: 1, Profile: "impl-g", Conflicted: "abc"}, {ID: "m2", Kind: "review", Issue: 1, PR: 10}},
				PRs:     []PR{{Number: 10, Head: "agent/1-x", Issue: 1, HeadSHA: "abc", Conflicting: true}}}, nil},
		{"PR on a non-fleet branch is ignored",
			State{GH: []GHIssue{open(1, "agent-ready", "wave:ui")},
				Multica: []MIssue{{ID: "m1", Kind: "task", Issue: 1, Profile: "impl-g"}},
				PRs:     []PR{{Number: 10, Head: "feature/x", Issue: 0}}}, nil},
		{"stuck issue gets no more actions",
			State{GH: []GHIssue{open(1, "agent-ready", "wave:ui", "agent-stuck")},
				Multica: []MIssue{{ID: "m1", Kind: "task", Issue: 1, Profile: "impl-g"}},
				PRs:     []PR{{Number: 10, Head: "agent/1-x", Issue: 1, Gate: []GateRun{{ID: "r1", Conclusion: "failure"}}}}}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := kinds(Plan(f, tt.st))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got  %q\nwant %q", got, tt.want)
			}
		})
	}
}

func TestReviewerSameVendorWhenRuleOff(t *testing.T) {
	f := fleet()
	f.Routing.CrossVendorReview = false
	// PR 11: candidates sorted [rev-a rev-g], 11%2=1 → rev-g, same vendor as impl-g: allowed.
	r, ok := routeReviewer(f, "impl-g", 11)
	if !ok || r != "rev-g" {
		t.Errorf("got %s %v", r, ok)
	}
	f.Routing.CrossVendorReview = true
	r, _ = routeReviewer(f, "impl-g", 11)
	if r != "rev-a" {
		t.Errorf("cross-vendor: got %s", r)
	}
}

func TestDependsOn(t *testing.T) {
	tests := []struct {
		body string
		want []int
	}{
		{"", nil},
		{"no deps here #5", nil},
		{"x\n\n## Depends on\n- #3\n- #7\n", []int{3, 7}},
		{"## Depends on\n#3 #3 #4\n\n## Notes\n#9", []int{3, 4}},
		{"Depends on: #12, #2", []int{12, 2}},
		{"### depends on (blocking)\nsee #1\n# Other\n#2", []int{1}},
	}
	for _, tt := range tests {
		if got := DependsOn(tt.body); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("DependsOn(%q) = %v, want %v", tt.body, got, tt.want)
		}
	}
}

func TestIssueFromBranch(t *testing.T) {
	for in, want := range map[string]int{"agent/12-wallet-freeze": 12, "agent/7": 7, "agent/x-1": 0, "main": 0, "fix/12-x": 0} {
		if got := IssueFromBranch(in); got != want {
			t.Errorf("IssueFromBranch(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestEscalationFor(t *testing.T) {
	L := fleet().Labels
	label, body := escalationFor("I need the OpenAPI contract → needs-contract", L)
	if label != "needs-contract" || !strings.Contains(body, "OpenAPI") || !strings.Contains(body, "remove the `needs-contract` label") {
		t.Errorf("got %s / %q", label, body)
	}
	if label, _ := escalationFor("", L); label != "needs-human" {
		t.Errorf("default label %s", label)
	}
}

func TestAttributionLines(t *testing.T) {
	msg := "feat: x [#1]\n\nCloses #1\n\nCo-authored-by: multica-agent <github@multica.ai>\nSigned-off-by: Emmanuel Yeboah <e@x>\n🤖 Generated with Claude Code\nClaude-Session: https://x"
	got := AttributionLines(msg)
	want := []string{"Co-authored-by: multica-agent <github@multica.ai>", "🤖 Generated with Claude Code", "Claude-Session: https://x"}
	if reflect.DeepEqual(got, want) == false {
		t.Errorf("got %q, want %q", got, want)
	}
	if AttributionLines("feat: plain\n\nCo-authored-by: Jane Doe <jane@example.com>") != nil {
		t.Error("human co-author flagged")
	}
}

func TestBodyErrors(t *testing.T) {
	if got := BodyErrors("Closes #3\n\n## What\n\nModel: impl-g", 3, "impl-g"); got != nil {
		t.Errorf("conforming body: %v", got)
	}
	got := BodyErrors("Adds greetAll.\n\nModel: impl-a", 3, "impl-g")
	if len(got) != 2 || !strings.Contains(got[0], "Closes #3") || !strings.Contains(got[1], "should be `Model: impl-g`") {
		t.Errorf("got %v", got)
	}
	if got := BodyErrors("closes #3 and fixes it", 3, ""); len(got) != 1 || !strings.Contains(got[0], "Model") {
		t.Errorf("got %v", got)
	}
}
