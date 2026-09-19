package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/noelzappy/fleet/internal/config"
	"github.com/noelzappy/fleet/internal/fleetsync"
	"github.com/noelzappy/fleet/internal/shell"
)

// tone is how a row should look: the renderer maps it to a colour, the data code decides
// it, so the rules can be tested without a terminal.
type tone int

const (
	toneBad tone = iota // sort order too: what needs you comes first
	toneWarn
	toneInfo
	toneGood
	toneAccent
	toneDim
)

// snapshot is everything one refresh learned. It is read-only observation: collecting it
// never writes to GitHub, Multica or the cooldown file.
type snapshot struct {
	At      time.Time
	Took    time.Duration
	Project string
	Repo    string
	Daemon  string
	Timer   string
	Paused  bool
	State   fleetsync.State
	Plan    []fleetsync.Action
	Issues  []issueRow
	PRs     []prRow
	Harness []harnessRow
	Logs    [2][]string // sync, daemon: newest last
}

type issueRow struct {
	Num   int
	Title string
	State string
	Tone  tone
	Agent string
	PR    int
	Gate  string
	Why   string // why a ready issue isn't being dispatched; shown under the row
}

type prRow struct {
	Num   int
	Issue int
	Head  string
	Gate  string
	Tone  tone
	Flags []string
}

type harnessRow struct {
	Name   string
	Status string // ok | signed out | cooling until 15:04
	Tone   tone
}

// collect observes the fleet once. Failures of individual extras (service state, logs)
// degrade to blanks; a failure of the core observation is returned so the UI keeps the
// previous snapshot on screen and says why it is stale.
func collect(ctx context.Context) (*snapshot, error) {
	start := time.Now()
	st, err := observe(ctx, false)
	if err != nil {
		return nil, err
	}
	s := &snapshot{At: time.Now(), Project: cfg.Project.Name, Repo: cfg.Project.Repo, State: st}
	p := box()
	abs, _ := filepath.Abs(cfgPath)
	jobs := p.Jobs(serviceSpec(abs))
	s.Daemon, _ = shell.Output(ctx, p.IsActiveCmd(jobs[0]))
	s.Timer, _ = shell.Output(ctx, p.IsActiveCmd(jobs[1]))
	for _, is := range st.GH {
		if is.State == "OPEN" && hasLabel(is.Labels, cfg.Labels.Paused) {
			s.Paused = true
		}
	}
	s.Plan = fleetsync.Plan(cfg, st)
	s.Issues = buildIssueRows(cfg, st)
	s.PRs = buildPRRows(st)
	s.Harness = buildHarnessRows(cfg, st)
	s.Logs[0] = logLines(ctx, jobs[1], 200)
	s.Logs[1] = logLines(ctx, jobs[0], 200)
	s.Took = time.Since(start)
	return s, nil
}

func hasLabel(labels []string, l string) bool {
	for _, x := range labels {
		if x == l {
			return true
		}
	}
	return false
}

// gateSummary describes a PR's newest gate run: pass, fail (with the failing jobs), or
// running when GitHub hasn't concluded it yet. "" when the gate hasn't run.
func gateSummary(pr fleetsync.PR) (string, tone) {
	if len(pr.Gate) == 0 {
		return "", toneDim
	}
	switch g := pr.Gate[0]; g.Conclusion {
	case "success":
		return "✓ pass", toneGood
	case "failure":
		if len(g.FailedJobs) > 0 {
			return "✗ " + strings.Join(g.FailedJobs, ","), toneBad
		}
		return "✗ fail", toneBad
	case "":
		return "… running", toneInfo
	default:
		return g.Conclusion, toneWarn
	}
}

// buildIssueRows joins each open GitHub issue with the Multica task that mirrors it and
// its PR, and names its state in the operator's terms. Rows that need a human sort first.
func buildIssueRows(f *config.Fleet, st fleetsync.State) []issueRow {
	L := f.Labels
	stalled := fleetsync.Stalled(f, st)
	tasks := map[int]fleetsync.MIssue{}
	for _, m := range st.Multica {
		if m.Kind != fleetsync.KindTask || m.Status == fleetsync.StatusCancelled {
			continue
		}
		tasks[m.Issue] = m
	}
	prs := map[int]fleetsync.PR{}
	for _, pr := range st.PRs {
		if pr.Issue != 0 {
			prs[pr.Issue] = pr
		}
	}
	var rows []issueRow
	for _, is := range st.GH {
		if is.State != "OPEN" {
			continue
		}
		row := issueRow{Num: is.Number, Title: is.Title}
		m, mirrored := tasks[is.Number]
		pr, hasPR := prs[is.Number]
		if mirrored {
			row.Agent = m.Profile
			if m.RunActive {
				row.Agent += " ▶"
			}
		}
		if hasPR {
			row.PR = pr.Number
		}
		var gate tone
		if hasPR {
			row.Gate, gate = gateSummary(pr)
		}
		switch {
		case hasLabel(is.Labels, L.Stuck):
			row.State, row.Tone = "stuck", toneBad
		case hasLabel(is.Labels, L.NeedsHuman):
			row.State, row.Tone = L.NeedsHuman, toneWarn
		case hasLabel(is.Labels, L.NeedsResource):
			row.State, row.Tone = L.NeedsResource, toneWarn
		case hasLabel(is.Labels, L.NeedsContract):
			row.State, row.Tone = L.NeedsContract, toneWarn
		case hasLabel(is.Labels, L.HumanRequired):
			row.State, row.Tone = "human only", toneDim
		case mirrored && fleetsync.AgentFailed(m):
			row.State, row.Tone = "agent failed", toneBad
		case hasPR && gate == toneBad:
			row.State, row.Tone = "gate failing", toneBad
		case mirrored && m.RunActive:
			row.State, row.Tone = "working", toneInfo
		case hasPR:
			row.State, row.Tone = "in review", toneGood
		case mirrored && m.Status == fleetsync.StatusBlocked:
			row.State, row.Tone = "blocked", toneWarn
		case mirrored:
			row.State, row.Tone = "queued", toneAccent
		case hasLabel(is.Labels, L.BlockedBy):
			row.State, row.Tone = "waiting on deps", toneDim
		case stalled[is.Number] != "":
			row.State, row.Tone, row.Why = "ready · stalled", toneWarn, stalled[is.Number]
		case hasLabel(is.Labels, L.Ready):
			row.State, row.Tone = "ready", toneAccent
		default:
			row.State, row.Tone = "open", toneDim
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Tone != rows[j].Tone {
			return rows[i].Tone < rows[j].Tone
		}
		return rows[i].Num < rows[j].Num
	})
	return rows
}

func buildPRRows(st fleetsync.State) []prRow {
	var rows []prRow
	for _, pr := range st.PRs {
		r := prRow{Num: pr.Number, Issue: pr.Issue, Head: pr.Head}
		r.Gate, r.Tone = gateSummary(pr)
		if pr.Conflicting {
			r.Flags = append(r.Flags, "conflicts")
		}
		if len(pr.Attribution) > 0 {
			r.Flags = append(r.Flags, "attribution trailer")
		}
		if len(pr.BodyErrors) > 0 {
			r.Flags = append(r.Flags, "body: "+strings.Join(pr.BodyErrors, "; "))
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Num < rows[j].Num })
	return rows
}

func buildHarnessRows(f *config.Fleet, st fleetsync.State) []harnessRow {
	names := make([]string, 0, len(f.Harnesses))
	for n := range f.Harnesses {
		names = append(names, n)
	}
	sort.Strings(names)
	var rows []harnessRow
	for _, n := range names {
		r := harnessRow{Name: n, Status: "signed in", Tone: toneGood}
		if until, ok := st.Cooling[n]; ok {
			r.Status, r.Tone = "out of quota until "+until.Local().Format("15:04"), toneWarn
		} else if st.SignedOut[n] {
			r.Status, r.Tone = "signed out", toneBad
		}
		rows = append(rows, r)
	}
	return rows
}

// logLines returns the last n lines of a job's log, newest last: the launchd log file on
// macOS, the user journal on Linux.
func logLines(ctx context.Context, job string, n int) []string {
	var raw string
	if _, err := requireBox("watch"); err != nil {
		return nil
	}
	if path := config.ExpandPath("~/Library/Logs/fleet/" + job + ".log"); fileExists(path) {
		raw = tailFile(path, 64<<10)
	} else if out, err := shell.Output(ctx, fmt.Sprintf("journalctl --user -u %s -n %d --no-pager -o cat 2>/dev/null", shell.Quote(job), n)); err == nil {
		raw = out
	}
	lines := strings.Split(strings.TrimRight(raw, "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// tailFile reads at most the last max bytes of a file, dropping a cut first line.
func tailFile(path string, max int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	off := int64(0)
	if fi.Size() > max {
		off = fi.Size() - max
	}
	buf := make([]byte, fi.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && len(buf) > 0 {
		return ""
	}
	s := string(buf)
	if off > 0 {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
	}
	return s
}
