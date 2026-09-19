package fleetsync

import (
	"strings"
	"testing"

	"github.com/noelzappy/fleet/internal/config"
)

func stalledFleet() *config.Fleet {
	f := &config.Fleet{
		Waves:     []config.Wave{{Name: "backend"}, {Name: "ui"}},
		Harnesses: map[string]config.Harness{"cc": {Kind: "claude-code"}, "ag": {Kind: "antigravity"}},
		Profiles: map[string]config.Profile{
			"impl-cc": {Harness: "cc", Role: config.RoleImplementer, Concurrency: 1, Waves: []string{"backend"}},
			"impl-ag": {Harness: "ag", Role: config.RoleImplementer, Concurrency: 1, Waves: []string{"backend", "ui"}},
		},
	}
	f.ApplyDefaults()
	return f
}

func TestStalled(t *testing.T) {
	f := stalledFleet()
	ready := f.Labels.Ready
	tests := []struct {
		name string
		st   State
		want map[int]string // issue → substring of the reason; absent = not stalled
	}{
		{"dispatches fine", State{GH: []GHIssue{{Number: 1, State: "OPEN", Labels: []string{ready, "wave:backend"}}}}, nil},
		{"no wave label", State{GH: []GHIssue{{Number: 1, State: "OPEN", Labels: []string{ready}}}},
			map[int]string{1: "add one of wave:backend, wave:ui"}},
		{"paused", State{GH: []GHIssue{
			{Number: 1, State: "OPEN", Labels: []string{ready, "wave:backend"}},
			{Number: 2, State: "OPEN", Labels: []string{f.Labels.Paused}}}},
			map[int]string{1: "paused"}},
		{"open dependency", State{GH: []GHIssue{
			{Number: 1, State: "OPEN", Labels: []string{ready, "wave:backend"}, Body: "## Depends on\n- #2\n"},
			{Number: 2, State: "OPEN"}}},
			map[int]string{1: "waiting for #2"}},
		{"only signed-out harness serves the wave", State{
			GH:        []GHIssue{{Number: 1, State: "OPEN", Labels: []string{ready, "wave:ui"}}},
			SignedOut: map[string]bool{"ag": true}},
			map[int]string{1: "signed-out or out-of-quota harness (ag)"}},
		{"another harness still serves it", State{
			GH:        []GHIssue{{Number: 1, State: "OPEN", Labels: []string{ready, "wave:backend"}}},
			SignedOut: map[string]bool{"ag": true}}, nil},
		{"already mirrored", State{
			GH:      []GHIssue{{Number: 1, State: "OPEN", Labels: []string{ready}}},
			Multica: []MIssue{{Kind: KindTask, Issue: 1, Status: "todo"}}}, nil},
		{"a label already explains it", State{GH: []GHIssue{{Number: 1, State: "OPEN", Labels: []string{ready, f.Labels.NeedsHuman}}}}, nil},
		{"not ready at all", State{GH: []GHIssue{{Number: 1, State: "OPEN"}}}, nil},
		{"closed", State{GH: []GHIssue{{Number: 1, State: "CLOSED", Labels: []string{ready}}}}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Stalled(f, tt.st)
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want reasons for %v", got, tt.want)
			}
			for n, sub := range tt.want {
				if !strings.Contains(got[n], sub) {
					t.Errorf("#%d: %q doesn't contain %q", n, got[n], sub)
				}
			}
		})
	}
}

// Stalled and Plan must agree: whatever Plan dispatches is never called stalled, and a
// stalled issue is never dispatched.
func TestStalledAgreesWithPlan(t *testing.T) {
	f := stalledFleet()
	st := State{GH: []GHIssue{
		{Number: 1, State: "OPEN", Labels: []string{f.Labels.Ready, "wave:backend"}},
		{Number: 2, State: "OPEN", Labels: []string{f.Labels.Ready}},
		{Number: 3, State: "OPEN", Labels: []string{f.Labels.Ready, "wave:ui"}},
	}, SignedOut: map[string]bool{"ag": true}}
	stalled := Stalled(f, st)
	for _, a := range Plan(f, st) {
		if a.Kind == "create-task" {
			if _, bad := stalled[a.Issue.Number]; bad {
				t.Errorf("#%d is dispatched and stalled: %s", a.Issue.Number, stalled[a.Issue.Number])
			}
		}
	}
	if _, ok := stalled[1]; ok {
		t.Error("#1 dispatches; must not be stalled")
	}
	if len(stalled) != 2 {
		t.Errorf("stalled = %v, want #2 (no wave) and #3 (harness down)", stalled)
	}
}
