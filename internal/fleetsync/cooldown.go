package fleetsync

import (
	"regexp"
	"strings"
	"time"
)

// A vendor that refuses work for lack of quota is not signed out: its auth check still
// passes. Left alone, routing keeps handing it issues that fail the same way. So a failed
// run whose error reads like a usage or rate limit puts its harness on a cooldown, and
// routing treats a cooling harness like a signed-out one until it expires.

var quotaWords = []string{
	"usage limit", "session limit", "rate limit", "rate_limit", "ratelimit",
	"quota", "resource_exhausted", "insufficient_quota", "credit balance",
	"too many requests", "limit reached", "limit exceeded", "exceeded your",
	"you've hit your limit", "you have hit your limit",
}

var quotaStatus = regexp.MustCompile(`(?i)(http|status|error|code)[ :=]*429\b|\b429 too many`)

// QuotaError reports whether a failed run's error text says the vendor refused the work
// because a usage, session or rate limit was reached.
func QuotaError(errText string) bool {
	s := strings.ToLower(errText)
	for _, w := range quotaWords {
		if strings.Contains(s, w) {
			return true
		}
	}
	return quotaStatus.MatchString(errText)
}

// Cooldown is one harness's pause. Run is the failed run that started it: a failed run
// stays on its issue until the owner clears it, and must not re-arm the cooldown each
// time the previous one expires.
type Cooldown struct {
	Until time.Time `json:"until"`
	Run   string    `json:"run"`
}

// UpdateCooldowns folds this tick's failed runs into the table: each quota-failed run not
// yet recorded starts (or extends) its harness's cooldown. harnessOf maps a profile name
// to its harness; the bool reports whether the table changed, so the caller writes it back
// only when needed. Expired entries stay: they remember which run started them, so a failed
// run left on its issue can't re-arm the cooldown. There is one entry per harness at most.
func UpdateCooldowns(table map[string]Cooldown, ms []MIssue, harnessOf func(profile string) string, now time.Time, d time.Duration) (map[string]Cooldown, bool) {
	out := make(map[string]Cooldown, len(table))
	for h, c := range table {
		out[h] = c
	}
	changed := false
	for _, m := range ms {
		if !AgentFailed(m) || !QuotaError(m.LastRunError) {
			continue
		}
		h := harnessOf(m.Profile)
		if h == "" || out[h].Run == m.LastRunID {
			continue
		}
		out[h] = Cooldown{Until: now.Add(d), Run: m.LastRunID}
		changed = true
	}
	return out, changed
}

// Cooling returns the harnesses whose cooldown hasn't expired.
func Cooling(table map[string]Cooldown, now time.Time) map[string]time.Time {
	out := map[string]time.Time{}
	for h, c := range table {
		if c.Until.After(now) {
			out[h] = c.Until
		}
	}
	return out
}
