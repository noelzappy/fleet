package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/noelzappy/fleet/internal/fleetsync"
	"github.com/noelzappy/fleet/internal/shell"
	"github.com/noelzappy/fleet/internal/ui"
	"github.com/spf13/cobra"
)

// fleet status — ≤ 8 lines, phone-readable.
func statusCmd() *cobra.Command {
	return &cobra.Command{Use: "status", Short: "service state, queue counts and open PRs in ≤ 8 lines", RunE: func(*cobra.Command, []string) error {
		return printStatus(context.Background(), os.Stdout)
	}}
}

// statusPalette styles for w: colour only when w is the terminal's stdout.
func statusPalette(w io.Writer) ui.Palette {
	if f, ok := w.(*os.File); ok {
		return ui.NewPalette(f)
	}
	return ui.NewPalette(io.Discard) // a buffer (tests, digest): plain text
}

// printStatus makes three calls in total (service, open issues with labels, open PRs)
// and counts labels locally, instead of one gh call per label.
func printStatus(ctx context.Context, w io.Writer) error {
	R, L := shell.Quote(cfg.Project.Repo), cfg.Labels
	p := box()
	abs, _ := filepath.Abs(cfgPath)
	j := p.Jobs(serviceSpec(abs))
	svc, _ := shell.Output(ctx, p.IsActiveCmd(j[0]))
	timer, _ := shell.Output(ctx, p.IsActiveCmd(j[1]))
	running, _ := shell.Output(ctx, fmt.Sprintf(`%s issue list --output json --status in_progress --metadata %s --fields id 2>/dev/null | grep -o '"id"' | wc -l | tr -d ' '`,
		multica, shell.Quote(fleetsync.MetaRepo+"="+cfg.Project.Repo)))
	issuesJSON, issuesErr := shell.Output(ctx, fmt.Sprintf(`gh issue list -R %s --state open --limit 1000 --json labels`, R))
	prs, prsErr := shell.Output(ctx, fmt.Sprintf(`gh pr list -R %s --state open --limit 1000 --json number -q 'length'`, R))

	counts, err := countLabels(issuesJSON)
	count := func(label string) string {
		if shell.DryRun || issuesErr != nil || err != nil {
			return "?"
		}
		return strconv.Itoa(counts[label])
	}
	if shell.DryRun || prsErr != nil {
		prs = "?"
	}
	for _, s := range []*string{&svc, &timer, &running} {
		if *s == "" {
			*s = "?"
		}
	}
	pal := statusPalette(w)
	// Keys are dim, values coloured by what they mean; the layout is unchanged so a phone
	// SSH session still gets three short lines plus warnings.
	kv := func(k, v string, style func(string) string) string { return pal.Dim.Render(k+": ") + style(v) }
	svcStyle := func(v string) string {
		switch v {
		case "active":
			return pal.Good.Render(v)
		case "loaded": // a timer between ticks
			return pal.Info.Render(v)
		}
		return pal.Bad.Render(v)
	}
	plain := func(v string) string { return pal.Bold.Render(v) }
	nonZero := func(style lipgloss.Style) func(string) string { // 0 is calm, anything else needs a look
		return func(v string) string {
			if v == "0" {
				return pal.Dim.Render(v)
			}
			return style.Render(v)
		}
	}
	fmt.Fprintln(w, kv("daemon", svc, svcStyle)+"   "+kv("sync", timer, svcStyle)+"   "+kv("running", running, plain)+"   "+kv("paused", count(L.Paused), nonZero(pal.Warn)))
	fmt.Fprintln(w, kv("ready", count(L.Ready), plain)+"   "+kv("stuck", count(L.Stuck), nonZero(pal.Bad))+"   "+kv("open PRs", prs, plain))
	fmt.Fprintln(w, kv("needs-human", count(L.NeedsHuman), nonZero(pal.Warn))+"   "+kv("needs-resource", count(L.NeedsResource), nonZero(pal.Warn))+"   "+kv("needs-contract", count(L.NeedsContract), nonZero(pal.Warn)))
	if cfg.GitHub.Auth == "app" {
		if err := requireAppIsolation(ctx); err != nil {
			fmt.Fprintln(w, pal.Bad.Render("github: UNSAFE — "+err.Error()))
		}
	} else {
		fmt.Fprintln(w, pal.Warn.Render("github: personal gh login (scratch repos only; github.auth: app before client code)"))
	}
	if out := signedOut(ctx, cfg.Harnesses); len(out) > 0 {
		var names []string
		for _, n := range sortedHarnessNames() {
			if out[n] {
				names = append(names, n)
			}
		}
		fmt.Fprintln(w, pal.Warn.Render("signed out: "+strings.Join(names, ", ")+" (no new work routed to their profiles)"))
	}
	// TODO(implementer): spend today per profile.
	if issuesErr != nil {
		return fmt.Errorf("gh issue list: %w", issuesErr)
	}
	if err != nil {
		return err
	}
	if prsErr != nil {
		return fmt.Errorf("gh pr list: %w", prsErr)
	}
	return nil
}

// countLabels parses `gh issue list --json labels` into label name → open issue count.
func countLabels(ghJSON string) (map[string]int, error) {
	counts := map[string]int{}
	if ghJSON == "" {
		return counts, nil
	}
	var issues []struct {
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := json.Unmarshal([]byte(ghJSON), &issues); err != nil {
		return nil, fmt.Errorf("parse gh issue list: %w", err)
	}
	for _, is := range issues {
		for _, l := range is.Labels {
			counts[l.Name]++
		}
	}
	return counts, nil
}

// fleet digest — status + merged-in-24h + gate pass rate, POSTed to Telegram if TG_TOKEN/TG_CHAT are set.
func digestCmd() *cobra.Command {
	return &cobra.Command{Use: "digest", Short: "daily summary (currently: status)", RunE: func(*cobra.Command, []string) error {
		// TODO(implementer): merged-in-24h, gate pass rate, Telegram POST — build order step 7.
		return printStatus(context.Background(), os.Stdout)
	}}
}
