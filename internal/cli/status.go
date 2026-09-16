package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/noelzappy/fleet/internal/shell"
	"github.com/spf13/cobra"
)

// fleet status — ≤ 8 lines, phone-readable.
func statusCmd() *cobra.Command {
	return &cobra.Command{Use: "status", Short: "service state, queue counts and open PRs in ≤ 8 lines", RunE: func(*cobra.Command, []string) error {
		return printStatus(context.Background(), os.Stdout)
	}}
}

// printStatus makes three calls in total (service, open issues with labels, open PRs)
// and counts labels locally, instead of one gh call per label.
func printStatus(ctx context.Context, w io.Writer) error {
	R, L := shell.Quote(cfg.Project.Repo), cfg.Labels
	svc, _ := shell.Output(ctx, `systemctl --user is-active `+shell.Quote(cfg.Orchestrator.ServiceName)+` || true`)
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
	if svc == "" {
		svc = "?"
	}
	fmt.Fprintf(w, "service: %s   paused: %s\n", svc, count(L.Paused))
	fmt.Fprintf(w, "ready: %s   stuck: %s   open PRs: %s\n", count(L.Ready), count(L.Stuck), prs)
	fmt.Fprintf(w, "needs-human: %s   needs-resource: %s   needs-contract: %s\n",
		count(L.NeedsHuman), count(L.NeedsResource), count(L.NeedsContract))
	// TODO(implementer): active sessions (`ao session list --json`); spend today per profile.
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
