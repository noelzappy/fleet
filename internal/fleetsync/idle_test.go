package fleetsync

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 19, 14, 30, 0, 0, time.UTC)

// idleTask is issue #7 mirrored to impl-g, whose last run completed at `ended`.
func idleTask(ended time.Time, mut func(*MIssue)) MIssue {
	m := MIssue{ID: "m7", Kind: KindTask, Issue: 7, Profile: "impl-g", Status: "in_progress",
		LastRunID: "r1", LastRunStatus: "completed", LastRunEnded: ended}
	if mut != nil {
		mut(&m)
	}
	return m
}

func idleState(m MIssue, prs []PR, labels ...string) State {
	return State{Now: t0, GH: []GHIssue{open(7, labels...)}, Multica: []MIssue{m}, PRs: prs}
}

func TestIdle(t *testing.T) {
	grace := 5 * time.Minute
	old := t0.Add(-10 * time.Minute)
	tests := []struct {
		name  string
		m     MIssue
		hasPR bool
		want  bool
	}{
		{"completed with no PR, past the grace", idleTask(old, nil), false, true},
		{"still inside the grace period", idleTask(t0.Add(-2*time.Minute), nil), false, false},
		{"exactly at the grace", idleTask(t0.Add(-grace), nil), false, true},
		{"it opened a PR", idleTask(old, nil), true, false},
		{"a run is active", idleTask(old, func(m *MIssue) { m.RunActive = true }), false, false},
		{"the run failed: that is the failure rule's", idleTask(old, func(m *MIssue) { m.LastRunStatus = "failed" }), false, false},
		{"the agent handed it back as blocked", idleTask(old, func(m *MIssue) { m.Status = "blocked" }), false, false},
		{"the agent marked it done", idleTask(old, func(m *MIssue) { m.Status = "done" }), false, false},
		{"unknown end time is never idle", idleTask(time.Time{}, nil), false, false},
		{"a review, not a task", idleTask(old, func(m *MIssue) { m.Kind = KindReview }), false, false},
		{"no run yet", idleTask(old, func(m *MIssue) { m.LastRunID = "" }), false, false},
	}
	for _, tt := range tests {
		if got := Idle(tt.m, tt.hasPR, t0, grace); got != tt.want {
			t.Errorf("%s: Idle = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func idleActions(t *testing.T, st State) []Action {
	t.Helper()
	var out []Action
	for _, a := range Plan(fleet(), st) {
		if a.Kind == "nudge-idle" || a.Kind == "escalate-idle" || a.Kind == "retry" {
			out = append(out, a)
		}
	}
	return out
}

func TestPlanIdleNudgesThenEscalates(t *testing.T) {
	old := t0.Add(-10 * time.Minute)

	// First idle end: one reminder that names the gate command and the PR contract.
	acts := idleActions(t, idleState(idleTask(old, nil), nil))
	if len(acts) != 1 || acts[0].Kind != "nudge-idle" || acts[0].Profile != "impl-g" {
		t.Fatalf("want one nudge-idle to impl-g, got %v", kinds(acts))
	}
	for _, want := range []string{"@impl-g", "FOREGROUND", "pnpm gate", "Model: impl-g", "pull request"} {
		if !strings.Contains(acts[0].Comment, want) {
			t.Errorf("nudge text missing %q:\n%s", want, acts[0].Comment)
		}
	}

	// The same run isn't nudged again on the next tick (metadata records it).
	handled := idleTask(old, func(m *MIssue) { m.IdleNudgedRun, m.IdleNudges = "r1", 1 })
	if acts := idleActions(t, idleState(handled, nil)); len(acts) != 0 {
		t.Errorf("a handled run was acted on again: %v", kinds(acts))
	}

	// After the reminder it ended again (a new run): escalate to the owner, once.
	again := idleTask(old, func(m *MIssue) { m.LastRunID, m.IdleNudgedRun, m.IdleNudges = "r2", "r1", 1 })
	acts = idleActions(t, idleState(again, nil))
	if len(acts) != 1 || acts[0].Kind != "escalate-idle" || acts[0].Label != "needs-human" {
		t.Fatalf("want escalate-idle +needs-human, got %v", kinds(acts))
	}
	if !strings.Contains(acts[0].Comment, "2 runs in a row") || !strings.Contains(acts[0].Comment, "needs-human") {
		t.Errorf("escalation text:\n%s", acts[0].Comment)
	}

	// Once labelled needs-human the fleet waits for the owner.
	if acts := idleActions(t, idleState(again, nil, "needs-human")); len(acts) != 0 {
		t.Errorf("acted while the owner has the issue: %v", kinds(acts))
	}

	// The owner removes the label: retry the same agent, once per run.
	escalated := idleTask(old, func(m *MIssue) { m.LastRunID, m.IdleNudgedRun, m.IdleNudges, m.IdleEsc = "r2", "r1", 1, "r2" })
	acts = idleActions(t, idleState(escalated, nil))
	if len(acts) != 1 || acts[0].Kind != "retry" || acts[0].Profile != "impl-g" {
		t.Fatalf("want retry after the label is removed, got %v", kinds(acts))
	}
	escalated.RerunOf = "r2"
	if acts := idleActions(t, idleState(escalated, nil)); len(acts) != 0 {
		t.Errorf("retry repeated for the same run: %v", kinds(acts))
	}

	// If its harness has since signed out, the retry is re-routed to one that is signed in.
	out := idleState(escalated2(old), nil, "wave:backend") // impl-a (claude) also serves backend
	out.SignedOut = map[string]bool{"agy": true}
	acts = idleActions(t, out)
	if len(acts) != 1 || acts[0].Kind != "retry" || acts[0].Profile != "impl-a" {
		t.Errorf("want a re-routed retry to impl-a, got %v", kinds(acts))
	}
}

func escalated2(old time.Time) MIssue {
	return idleTask(old, func(m *MIssue) { m.LastRunID, m.IdleNudgedRun, m.IdleNudges, m.IdleEsc = "r2", "r1", 1, "r2" })
}

func TestPlanIdleStaysQuiet(t *testing.T) {
	old := t0.Add(-10 * time.Minute)
	pr := []PR{{Number: 70, Issue: 7}}
	cases := map[string]State{
		"inside the grace period": idleState(idleTask(t0.Add(-time.Minute), nil), nil),
		"a PR exists":             idleState(idleTask(old, nil), pr),
		"the issue is stuck":      idleState(idleTask(old, nil), nil, "agent-stuck"),
		"the issue is closed": func() State {
			s := idleState(idleTask(old, nil), nil)
			s.GH[0].State = "CLOSED"
			return s
		}(),
		"fleet is paused": func() State {
			s := idleState(idleTask(old, nil), nil)
			s.GH = append(s.GH, open(99, "fleet-paused"))
			return s
		}(),
		"a cancelled task no longer mirrors the issue": idleState(idleTask(old, func(m *MIssue) { m.Status = StatusCancelled }), nil),
	}
	for name, st := range cases {
		if acts := idleActions(t, st); len(acts) != 0 {
			t.Errorf("%s: acted: %v", name, kinds(acts))
		}
	}
}

func TestIdleGrace(t *testing.T) {
	f := fleet()
	if got := IdleGrace(f); got != 5*time.Minute {
		t.Errorf("default = %v", got)
	}
	f.Routing.IdleGrace = "20m"
	if got := IdleGrace(f); got != 20*time.Minute {
		t.Errorf("configured = %v", got)
	}
	f.Routing.IdleGrace = "nonsense"
	if got := IdleGrace(f); got != DefaultIdleGrace {
		t.Errorf("unparseable should fall back, got %v", got)
	}
}
