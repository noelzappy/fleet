package fleetsync

import (
	"fmt"
	"time"

	"github.com/noelzappy/fleet/internal/config"
)

// An agent can end its session without finishing: it starts the gate in the background
// and says it will wait, or it stops after a partial edit. Multica records that run as
// `completed`, not `failed`, so the failure rules never see it, and the issue sits in
// in_progress with no one working on it. This file is the rule that notices.

// DefaultIdleGrace is how long a completed run may leave an issue without a PR before fleet
// acts: long enough for the PR to appear and for Multica's status to catch up.
const DefaultIdleGrace = 5 * time.Minute

// IdleGrace is routing.idle_grace, or the default when it is unset or unparseable.
func IdleGrace(f *config.Fleet) time.Duration {
	if d, err := time.ParseDuration(f.Routing.IdleGrace); err == nil && d > 0 {
		return d
	}
	return DefaultIdleGrace
}

// Idle reports a task whose agent ended its run cleanly without opening a PR, and did not
// hand the issue back as blocked or done. now is passed in so the rule stays pure.
func Idle(m MIssue, hasPR bool, now time.Time, grace time.Duration) bool {
	return m.Kind == KindTask && m.Status == "in_progress" && !m.RunActive && !hasPR &&
		m.LastRunStatus == "completed" && m.LastRunID != "" &&
		!m.LastRunEnded.IsZero() && now.Sub(m.LastRunEnded) >= grace
}

func idleNudgeText(profile, gate string) string {
	return fmt.Sprintf("@%s Your last run ended without a pull request. A session closes when you stop, so anything you started in the background (for example `%s`) was left unfinished, and nothing is waiting on it.\n\n"+
		"Your changes are still in your worktree. Please:\n"+
		"1. Run `%s` in the FOREGROUND and wait for it to finish; never background it.\n"+
		"2. Fix whatever it reports.\n"+
		"3. Commit, push the branch, and open the PR as AGENTS.md describes (`Closes #N`, and the `Model: %s` line).\n\n"+
		"Do not end your turn until the PR exists, or you have set this issue to blocked and named the label you need.", profile, gate, gate, profile)
}

func idleEscalationText(profile string, runs int, ended time.Time, needsHuman string) string {
	return fmt.Sprintf("The %s agent ended %d runs in a row without opening a PR (latest ended %s), including one after a reminder. Its work may be uncommitted in its worktree under the Multica runs directory.\n\n"+
		"Look at what it left, then either split or clarify the issue, or remove the `%s` label to give it another run.",
		profile, runs, ended.Local().Format("15:04 Mon"), needsHuman)
}
