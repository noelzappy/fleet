package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/noelzappy/fleet/internal/fleetsync"
	"github.com/noelzappy/fleet/internal/shell"
	"github.com/spf13/cobra"
)

// fleet sync — one reconciliation tick between GitHub and Multica. The systemd timer
// that `orchestrator init` installs runs it every orchestrator.sync_interval; it is
// also safe to run by hand. Rules live in internal/fleetsync; this file only observes
// and applies. See docs/orchestrator-decision.md › Closing the gaps.
func syncCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "one tick: mirror eligible GitHub issues into Multica, Multica state back as labels",
		RunE: func(*cobra.Command, []string) error {
			ctx := context.Background()
			st, err := observe(ctx)
			if err != nil {
				return err
			}
			actions := fleetsync.Plan(cfg, st)
			if len(actions) == 0 {
				fmt.Fprintln(os.Stderr, "sync: nothing to do")
				return nil
			}
			for _, a := range actions {
				fmt.Fprintln(os.Stderr, "● "+a.String())
				if err := apply(ctx, a); err != nil {
					return fmt.Errorf("%s: %w", a, err)
				}
			}
			return nil
		},
	}
}

const multica = "multica" // on PATH; orchestrator init installs it

// observe gathers the tick's inputs: every GitHub issue (closed ones decide
// dependencies), fleet's Multica issues, and open PRs with their gate runs.
func observe(ctx context.Context) (fleetsync.State, error) {
	var st fleetsync.State
	R := shell.Quote(cfg.Project.Repo)

	out, err := shell.Output(ctx, "gh issue list -R "+R+" --state all --limit 1000 --json number,title,body,labels,state,url")
	if err != nil {
		return st, fmt.Errorf("gh issue list: %w", err)
	}
	var gh []struct {
		Number                  int
		Title, Body, State, URL string
		Labels                  []struct{ Name string }
	}
	if err := decode(out, &gh); err != nil {
		return st, fmt.Errorf("gh issue list: %w", err)
	}
	for _, g := range gh {
		is := fleetsync.GHIssue{Number: g.Number, Title: g.Title, Body: g.Body, State: g.State, URL: g.URL}
		for _, l := range g.Labels {
			is.Labels = append(is.Labels, l.Name)
		}
		st.GH = append(st.GH, is)
	}

	filter := " --metadata " + shell.Quote(fleetsync.MetaRepo+"="+cfg.Project.Repo)
	for offset := 0; ; offset += 100 {
		out, err := shell.Output(ctx, fmt.Sprintf("%s issue list --output json --limit 100 --offset %d%s", multica, offset, filter))
		if err != nil {
			return st, fmt.Errorf("multica issue list: %w", err)
		}
		rows, more, err := jsonRows(out, "issues")
		if err != nil {
			return st, fmt.Errorf("multica issue list: %w", err)
		}
		for _, row := range rows {
			md, _ := row["metadata"].(map[string]any)
			m := fleetsync.MIssue{
				ID:         str(row["id"]),
				Status:     str(row["status"]),
				Kind:       str(md[fleetsync.MetaKind]),
				Issue:      num(md[fleetsync.MetaIssue]),
				PR:         num(md[fleetsync.MetaPR]),
				Profile:    str(md[fleetsync.MetaProfile]),
				NudgedRun:  str(md[fleetsync.MetaNudgedID]),
				Conflicted: str(md[fleetsync.MetaConflict]),
				Attributed: str(md[fleetsync.MetaAttrib]),
				BodyNudged: num(md[fleetsync.MetaBody]),
				RerunOf:    str(md[fleetsync.MetaRerun]),
			}
			if m.Status == "todo" || m.Status == "in_progress" {
				if err := latestRun(ctx, &m); err != nil {
					return st, err
				}
			}
			if m.Kind == fleetsync.KindTask && m.Status == fleetsync.StatusBlocked {
				m.LastComment = lastComment(ctx, m.ID)
			}
			st.Multica = append(st.Multica, m)
		}
		if !more || len(rows) == 0 || shell.DryRun {
			break
		}
	}
	mirrored := map[int]bool{}
	taskProfile := map[int]string{}
	for _, m := range st.Multica {
		if m.Kind == fleetsync.KindTask && m.Status != fleetsync.StatusCancelled {
			mirrored[m.Issue] = true
			taskProfile[m.Issue] = m.Profile
		}
	}

	out, err = shell.Output(ctx, "gh pr list -R "+R+" --state open --limit 200 --json number,headRefName,headRefOid,url,mergeable,body")
	if err != nil {
		return st, fmt.Errorf("gh pr list: %w", err)
	}
	var prs []struct {
		Number                                        int
		HeadRefName, HeadRefOid, URL, Mergeable, Body string
	}
	if err := decode(out, &prs); err != nil {
		return st, fmt.Errorf("gh pr list: %w", err)
	}
	for _, p := range prs {
		pr := fleetsync.PR{Number: p.Number, Head: p.HeadRefName, HeadSHA: p.HeadRefOid, URL: p.URL,
			Issue: fleetsync.IssueFromBranch(p.HeadRefName), Conflicting: p.Mergeable == "CONFLICTING"}
		if mirrored[pr.Issue] {
			pr.Gate, err = gateRuns(ctx, pr.Head)
			if err != nil {
				return st, err
			}
			msgs, err := shell.Output(ctx, fmt.Sprintf("gh api repos/%s/pulls/%d/commits --paginate -q '.[].commit.message'", shell.Quote(cfg.Project.Repo), pr.Number))
			if err != nil {
				return st, fmt.Errorf("gh api pulls/%d/commits: %w", pr.Number, err)
			}
			pr.Attribution = fleetsync.AttributionLines(msgs)
			pr.BodyErrors = fleetsync.BodyErrors(p.Body, pr.Issue, taskProfile[pr.Issue])
		}
		st.PRs = append(st.PRs, pr)
	}
	return st, nil
}

// gateRuns lists the gate workflow's runs on a branch, newest first, with failed
// job names for the newest failure (one extra call, only when it failed).
func gateRuns(ctx context.Context, branch string) ([]fleetsync.GateRun, error) {
	out, err := shell.Output(ctx, fmt.Sprintf("gh run list -R %s --branch %s --workflow %s --limit 20 --json databaseId,conclusion",
		shell.Quote(cfg.Project.Repo), shell.Quote(branch), shell.Quote(cfg.Gate.Workflow)))
	if err != nil {
		return nil, fmt.Errorf("gh run list %s: %w", branch, err)
	}
	var rows []struct {
		DatabaseID int64
		Conclusion string
	}
	if err := decode(out, &rows); err != nil {
		return nil, fmt.Errorf("gh run list %s: %w", branch, err)
	}
	var runs []fleetsync.GateRun
	for i, r := range rows {
		run := fleetsync.GateRun{ID: strconv.FormatInt(r.DatabaseID, 10), Conclusion: r.Conclusion}
		if i == 0 && r.Conclusion == "failure" {
			jobs, err := shell.Output(ctx, fmt.Sprintf(`gh run view %s -R %s --json jobs -q '[.jobs[]|select(.conclusion=="failure")|.name]|join("\n")'`,
				run.ID, shell.Quote(cfg.Project.Repo)))
			if err != nil {
				return nil, fmt.Errorf("gh run view %s: %w", run.ID, err)
			}
			run.FailedJobs = nonEmpty(strings.Split(jobs, "\n"))
		}
		runs = append(runs, run)
	}
	return runs, nil
}

// latestRun fills the issue's newest run (the CLI lists newest first) and whether
// any run is active, which is what the stranded-run rule needs.
func latestRun(ctx context.Context, m *fleetsync.MIssue) error {
	out, err := shell.Output(ctx, multica+" issue runs "+shell.Quote(m.ID)+" --output json")
	if err != nil {
		return fmt.Errorf("multica issue runs %s: %w", m.ID, err)
	}
	rows, _, err := jsonRows(out, "runs", "tasks")
	if err != nil {
		return fmt.Errorf("multica issue runs %s: %w", m.ID, err)
	}
	for i, r := range rows {
		switch str(r["status"]) {
		case "queued", "dispatched", "running", "waiting_local_directory":
			m.RunActive = true
		}
		if i == 0 {
			m.LastRunID, m.LastRunStatus, m.LastRunError = str(r["id"]), str(r["status"]), str(r["error"])
		}
	}
	return nil
}

// lastComment returns the newest comment on a Multica issue, "" if none or on error
// (escalation then falls back to needs-human with a generic body).
func lastComment(ctx context.Context, id string) string {
	out, err := shell.Output(ctx, multica+" issue comment list "+shell.Quote(id)+" --output json --recent 1")
	if err != nil {
		return ""
	}
	rows, _, err := jsonRows(out, "comments")
	if err != nil || len(rows) == 0 {
		return ""
	}
	return str(rows[len(rows)-1]["content"])
}

func apply(ctx context.Context, a fleetsync.Action) error {
	R := shell.Quote(cfg.Project.Repo)
	N := strconv.Itoa(a.Issue.Number)
	switch a.Kind {
	case "create-task":
		id, err := createMultica(ctx, fmt.Sprintf("#%d %s", a.Issue.Number, a.Issue.Title), taskDescription(a), a.Profile)
		if err != nil {
			return err
		}
		return setMeta(ctx, id, map[string]string{
			fleetsync.MetaRepo: cfg.Project.Repo, fleetsync.MetaKind: fleetsync.KindTask,
			fleetsync.MetaIssue: N, fleetsync.MetaProfile: a.Profile,
		})
	case "create-review":
		id, err := createMultica(ctx, fmt.Sprintf("Review PR #%d for #%d", a.PR.Number, a.Issue.Number), reviewDescription(a), a.Profile)
		if err != nil {
			return err
		}
		return setMeta(ctx, id, map[string]string{
			fleetsync.MetaRepo: cfg.Project.Repo, fleetsync.MetaKind: fleetsync.KindReview,
			fleetsync.MetaIssue: N, fleetsync.MetaPR: strconv.Itoa(a.PR.Number), fleetsync.MetaProfile: a.Profile,
		})
	case "unblock":
		msg := fmt.Sprintf("Unblocked on GitHub: %s\nThe owner answered there. Read it with `gh issue view %s -R %s --comments`, then continue.", a.Issue.URL, N, cfg.Project.Repo)
		if err := multicaComment(ctx, a.Multica.ID, msg); err != nil {
			return err
		}
		return shell.Run(ctx, multica+" issue status "+shell.Quote(a.Multica.ID)+" todo", nil)
	case "escalate":
		if err := shell.Run(ctx, fmt.Sprintf("gh issue edit -R %s %s --add-label %s", R, N, shell.Quote(a.Label)), nil); err != nil {
			return err
		}
		return ghComment(ctx, N, a.Comment)
	case "stuck":
		if err := shell.Run(ctx, fmt.Sprintf("gh issue edit -R %s %s --add-label %s --remove-label %s", R, N, shell.Quote(a.Label), shell.Quote(cfg.Labels.Ready)), nil); err != nil {
			return err
		}
		if err := ghComment(ctx, N, a.Comment); err != nil {
			return err
		}
		return cancelMultica(ctx, a.Multica.ID)
	case "nudge":
		if err := multicaComment(ctx, a.Multica.ID, a.Comment); err != nil {
			return err
		}
		return setMeta(ctx, a.Multica.ID, map[string]string{fleetsync.MetaNudgedID: a.Run.ID})
	case "rebase":
		if err := multicaComment(ctx, a.Multica.ID, a.Comment); err != nil {
			return err
		}
		return setMeta(ctx, a.Multica.ID, map[string]string{fleetsync.MetaConflict: a.PR.HeadSHA})
	case "strip-attribution":
		if err := multicaComment(ctx, a.Multica.ID, a.Comment); err != nil {
			return err
		}
		return setMeta(ctx, a.Multica.ID, map[string]string{fleetsync.MetaAttrib: a.PR.HeadSHA})
	case "rerun":
		if err := shell.Run(ctx, multica+" issue rerun "+shell.Quote(a.Multica.ID)+" >/dev/null", nil); err != nil {
			return err
		}
		return setMeta(ctx, a.Multica.ID, map[string]string{fleetsync.MetaRerun: a.Multica.LastRunID})
	case "fix-body":
		if err := multicaComment(ctx, a.Multica.ID, a.Comment); err != nil {
			return err
		}
		return setMeta(ctx, a.Multica.ID, map[string]string{fleetsync.MetaBody: strconv.Itoa(a.PR.Number)})
	}
	return fmt.Errorf("unknown action %q", a.Kind)
}

// createMultica creates a Multica issue in todo (backlog would park it) assigned to
// the profile's agent, and returns its id.
func createMultica(ctx context.Context, title, desc, profile string) (string, error) {
	out, err := shell.OutputInput(ctx, fmt.Sprintf("%s issue create --output json --status todo --title %s --assignee %s --description-stdin",
		multica, shell.Quote(title), shell.Quote(profile)), desc)
	if err != nil {
		return "", fmt.Errorf("multica issue create: %w", err)
	}
	if shell.DryRun {
		return "<new-issue>", nil
	}
	var row map[string]any
	if err := decode(out, &row); err != nil {
		return "", fmt.Errorf("multica issue create: %w", err)
	}
	id := str(row["id"])
	if id == "" {
		return "", fmt.Errorf("multica issue create: no id in %q", out)
	}
	return id, nil
}

func setMeta(ctx context.Context, id string, kv map[string]string) error {
	for _, k := range sortedKeys(kv) {
		typ := "string"
		if _, err := strconv.Atoi(kv[k]); err == nil {
			typ = "number"
		}
		if err := shell.Run(ctx, fmt.Sprintf("%s issue metadata set %s --key %s --value %s --type %s >/dev/null", multica, shell.Quote(id), k, shell.Quote(kv[k]), typ), nil); err != nil {
			return err
		}
	}
	return nil
}

func multicaComment(ctx context.Context, id, body string) error {
	_, err := shell.OutputInput(ctx, multica+" issue comment add "+shell.Quote(id)+" --content-stdin", body)
	return err
}

func ghComment(ctx context.Context, n, body string) error {
	_, err := shell.OutputInput(ctx, fmt.Sprintf("gh issue comment -R %s %s --body-file -", shell.Quote(cfg.Project.Repo), n), body)
	return err
}

// cancelMultica stops every active run on a Multica issue and closes it.
func cancelMultica(ctx context.Context, id string) error {
	out, err := shell.Output(ctx, multica+" issue runs "+shell.Quote(id)+" --active --output json")
	if err != nil {
		return err
	}
	rows, _, err := jsonRows(out, "runs", "tasks")
	if err != nil {
		return err
	}
	for _, r := range rows {
		run := str(r["task_id"])
		if run == "" {
			run = str(r["id"])
		}
		if err := shell.Run(ctx, multica+" issue cancel-task "+shell.Quote(run)+" >/dev/null", nil); err != nil {
			return err
		}
	}
	return shell.Run(ctx, multica+" issue status "+shell.Quote(id)+" cancelled --no-start", nil)
}

func taskDescription(a fleetsync.Action) string {
	is := a.Issue
	return fmt.Sprintf(`%s

---
GitHub issue: %s
Branch: `+"`agent/%d-%s`"+` from `+"`%s`"+`. PR title ends with `+"`[#%d]`"+`; PR body contains `+"`Closes #%d`"+` and a `+"`Model: %s`"+` line.
Run `+"`%s`"+` before opening the PR. Follow AGENTS.md.
If you cannot proceed: set this issue to blocked with a comment that names the label you need (%s, %s or %s) and explains what is unclear, the options, and your recommendation.`,
		strings.TrimSpace(is.Body), is.URL, is.Number, slug(is.Title), cfg.Project.Branch, is.Number, is.Number, a.Profile,
		cfg.Gate.Command, cfg.Labels.NeedsHuman, cfg.Labels.NeedsResource, cfg.Labels.NeedsContract)
}

func reviewDescription(a fleetsync.Action) string {
	vendor := cfg.Profiles[a.Profile].Vendor
	return fmt.Sprintf(`Review PR %s, which implements %s.

Read the diff with `+"`gh pr diff %d -R %s`"+` and the issue's acceptance criteria. Check scope (only the paths the issue allows), tests for every acceptance criterion, and the "Not done" section.
Post exactly one review with `+"`gh pr review %d -R %s --comment|--request-changes --body-file -`"+`. Never approve; a human merges.
The review body must end with the line: `+"`Reviewed-by: %s (%s)`"+`
Then set this issue to done.`, a.PR.URL, a.Issue.URL, a.PR.Number, cfg.Project.Repo, a.PR.Number, cfg.Project.Repo, a.Profile, vendor)
}

var slugRE = regexp.MustCompile(`[^a-z0-9]+`)

func slug(title string) string {
	s := strings.Trim(slugRE.ReplaceAllString(strings.ToLower(title), "-"), "-")
	if len(s) > 40 {
		s = strings.TrimRight(s[:40], "-")
	}
	if s == "" {
		s = "task"
	}
	return s
}

// jsonRows accepts either a bare JSON array or an object holding the array under
// one of keys (plus an optional has_more), which is how Multica's CLI pages.
func jsonRows(out string, keys ...string) (rows []map[string]any, more bool, err error) {
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, false, nil
	}
	if strings.HasPrefix(out, "[") {
		err = json.Unmarshal([]byte(out), &rows)
		return rows, false, err
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		return nil, false, err
	}
	for _, k := range keys {
		if list, ok := obj[k].([]any); ok {
			for _, it := range list {
				if m, ok := it.(map[string]any); ok {
					rows = append(rows, m)
				}
			}
			more, _ = obj["has_more"].(bool)
			return rows, more, nil
		}
	}
	return nil, false, fmt.Errorf("no %s array in %.80q", strings.Join(keys, "/"), out)
}

func decode(out string, v any) error {
	if strings.TrimSpace(out) == "" {
		return nil // --dry-run produces no output
	}
	return json.Unmarshal([]byte(out), v)
}

func str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return ""
}

func num(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case string:
		n, _ := strconv.Atoi(x)
		return n
	}
	return 0
}

func nonEmpty(list []string) []string {
	var out []string
	for _, s := range list {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
