package fleetsync

import (
	"testing"
	"time"
)

func TestQuotaError(t *testing.T) {
	yes := []string{
		"Claude usage limit reached. Your limit will reset at 3pm",
		"You've hit your limit · resets 5pm",
		"429 Too Many Requests",
		"API Error: status 429",
		"RESOURCE_EXHAUSTED: quota exceeded for model",
		"Your credit balance is too low to access the Anthropic API",
		"rate_limit_error: Number of request tokens has exceeded your per-minute rate limit",
		"ChatGPT usage limit hit; try again in 3 hours",
	}
	no := []string{
		"",
		"not logged in",
		"task cancelled by server",
		"exit status 1: gate failed",
		"panic at line 4290 of parser.go",
	}
	for _, s := range yes {
		if !QuotaError(s) {
			t.Errorf("QuotaError(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if QuotaError(s) {
			t.Errorf("QuotaError(%q) = true, want false", s)
		}
	}
}

func TestUpdateCooldowns(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	d := 5 * time.Hour
	harness := func(p string) string { return map[string]string{"claude-impl": "claude", "codex-rev": "codex"}[p] }
	failed := func(profile, run, err string) MIssue {
		return MIssue{Status: "in_progress", Profile: profile, LastRunID: run, LastRunStatus: "failed", LastRunError: err}
	}

	// A quota failure starts the harness's cooldown; other failures don't.
	tab, changed := UpdateCooldowns(nil, []MIssue{
		failed("claude-impl", "r1", "usage limit reached"),
		failed("codex-rev", "r2", "gate failed"),
	}, harness, now, d)
	if !changed || len(tab) != 1 || !tab["claude"].Until.Equal(now.Add(d)) || tab["claude"].Run != "r1" {
		t.Fatalf("table = %+v changed=%v", tab, changed)
	}
	if got := Cooling(tab, now.Add(time.Hour)); len(got) != 1 || got["claude"].IsZero() {
		t.Errorf("Cooling within window = %v", got)
	}

	// The same failed run, still on its issue, doesn't re-arm the cooldown after it expires.
	later := now.Add(6 * time.Hour)
	tab2, changed := UpdateCooldowns(tab, []MIssue{failed("claude-impl", "r1", "usage limit reached")}, harness, later, d)
	if changed || len(Cooling(tab2, later)) != 0 {
		t.Errorf("stale run re-armed cooldown: %+v changed=%v", tab2, changed)
	}

	// A new failed run does.
	tab3, changed := UpdateCooldowns(tab2, []MIssue{failed("claude-impl", "r9", "usage limit reached")}, harness, later, d)
	if !changed || !tab3["claude"].Until.Equal(later.Add(d)) {
		t.Errorf("new run should re-arm: %+v changed=%v", tab3, changed)
	}

	// The input table isn't mutated, and a running issue isn't a failure.
	if tab["claude"].Run != "r1" {
		t.Error("input table mutated")
	}
	running := failed("claude-impl", "r10", "usage limit reached")
	running.RunActive = true
	if _, changed := UpdateCooldowns(tab3, []MIssue{running}, harness, later, d); changed {
		t.Error("active run counted as failure")
	}
}
