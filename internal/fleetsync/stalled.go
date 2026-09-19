package fleetsync

import (
	"fmt"
	"sort"
	"strings"

	"github.com/noelzappy/fleet/internal/config"
)

// Stalled explains, per GitHub issue number, why a ready issue with no Multica task is not
// being dispatched. Plan skips these silently (an issue nobody can take is not an error),
// which leaves an operator staring at an `agent-ready` label that does nothing; this is the
// missing sentence. Issues that dispatch normally, or that a label already explains
// (stuck, needs-*, blocked-by, human-required), are absent from the result.
func Stalled(f *config.Fleet, st State) map[int]string {
	L := f.Labels
	byNum := map[int]GHIssue{}
	paused := false
	for _, is := range st.GH {
		byNum[is.Number] = is
		if is.State == "OPEN" && has(is.Labels, L.Paused) {
			paused = true
		}
	}
	mirrored := map[int]bool{}
	for _, m := range st.Multica {
		if m.Kind == KindTask && m.Status != StatusCancelled {
			mirrored[m.Issue] = true
		}
	}
	out := map[int]string{}
	for _, is := range st.GH {
		if is.State != "OPEN" || mirrored[is.Number] || !has(is.Labels, L.Ready) && !has(is.Labels, L.Assist) {
			continue
		}
		if hasAny(is.Labels, append(escalationLabels(L), L.BlockedBy, L.HumanRequired)) {
			continue
		}
		if why := stalledReason(f, is, byNum, st.SignedOut, paused); why != "" {
			out[is.Number] = why
		}
	}
	return out
}

func stalledReason(f *config.Fleet, is GHIssue, all map[int]GHIssue, out map[string]bool, paused bool) string {
	if paused {
		return "fleet is paused"
	}
	for _, dep := range DependsOn(is.Body) {
		if d, ok := all[dep]; !ok || d.State != "CLOSED" {
			return fmt.Sprintf("waiting for #%d to close", dep)
		}
	}
	wave := ""
	for _, l := range is.Labels {
		if w := f.WaveForLabel(l); w != "" {
			wave = w
			break
		}
	}
	if wave == "" {
		var labels []string
		for _, w := range f.Waves {
			labels = append(labels, w.WaveLabel())
		}
		return "no wave label, so no profile can take it: add one of " + strings.Join(labels, ", ")
	}
	serving := f.ProfilesWhere(func(_ string, p config.Profile) bool {
		return p.Role == config.RoleImplementer && p.Concurrency > 0 && contains(p.Waves, wave)
	})
	if len(serving) == 0 {
		return fmt.Sprintf("no implementer profile serves wave %q", wave)
	}
	var down []string
	for _, name := range serving {
		if h := f.Profiles[name].Harness; out[h] {
			down = append(down, h)
		}
	}
	if len(down) == len(serving) {
		sort.Strings(down)
		return fmt.Sprintf("every profile for wave %q is on a signed-out or out-of-quota harness (%s)", wave, strings.Join(dedupe(down), ", "))
	}
	return ""
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
