package cli

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/noelzappy/fleet/internal/fleetsync"
	"github.com/noelzappy/fleet/internal/shell"
	"github.com/spf13/cobra"
)

// up / pause / resume / kill / panic. Must work from a phone over SSH.
//
// Two systemd user units matter: <service> runs the Multica daemon (the agents) and
// <service>-sync.timer runs `fleet sync` (intake). The Multica server itself is docker
// compose with restart policies and is left running across pause/resume.

func units() string {
	s := cfg.Orchestrator.ServiceName
	return shell.Quote(s) + " " + shell.Quote(s+"-sync.timer")
}

func upCmd() *cobra.Command {
	return &cobra.Command{Use: "up", Short: "start the Multica server, daemon and sync timer", RunE: func(*cobra.Command, []string) error {
		if err := requireLinux("up"); err != nil {
			return err
		}
		ctx := context.Background()
		if err := shell.Run(ctx, "cd "+shell.Quote(cfg.Orchestrator.Dir)+" && docker compose -f docker-compose.selfhost.yml -f docker-compose.fleet.yml up -d", nil); err != nil {
			return err
		}
		return shell.Run(ctx, "systemctl --user enable --now "+units(), nil)
	}}
}

// pause: soft (default) opens a fleet-paused issue so sync mirrors nothing new while
// in-flight runs finish; --hard also stops the daemon (runs are killed after Multica's
// 30s grace; Multica re-queues them when the daemon returns).
func pauseCmd() *cobra.Command {
	var hard bool
	c := &cobra.Command{Use: "pause", Short: "stop dispatching (soft) or stop the agents now (--hard)", RunE: func(*cobra.Command, []string) error {
		ctx := context.Background()
		if hard {
			if err := requireLinux("pause --hard"); err != nil {
				return err
			}
			return shell.Run(ctx, "systemctl --user stop "+units(), nil)
		}
		body := "paused by fleet cli at " + time.Now().UTC().Format(time.RFC3339)
		return shell.Run(ctx, fmt.Sprintf(`gh issue create -R %s --title "FLEET PAUSED" --label %s --body %s`,
			shell.Quote(cfg.Project.Repo), shell.Quote(cfg.Labels.Paused), shell.Quote(body)), nil)
	}}
	c.Flags().BoolVar(&hard, "hard", false, "also stop the daemon and sync timer now")
	return c
}

func resumeCmd() *cobra.Command {
	return &cobra.Command{Use: "resume", Short: "close fleet-paused issues and start the daemon and sync timer", RunE: func(*cobra.Command, []string) error {
		if err := requireLinux("resume"); err != nil {
			return err
		}
		ctx := context.Background()
		nums, err := shell.Output(ctx, fmt.Sprintf(`gh issue list -R %s --label %s --state open --json number -q '.[].number'`, shell.Quote(cfg.Project.Repo), shell.Quote(cfg.Labels.Paused)))
		if err != nil {
			return err
		}
		for _, n := range strings.Fields(nums) {
			if err := shell.Run(ctx, fmt.Sprintf(`gh issue close -R %s %s`, shell.Quote(cfg.Project.Repo), shell.Quote(n)), nil); err != nil {
				return err
			}
		}
		return shell.Run(ctx, "systemctl --user start "+units(), nil)
	}}
}

// kill <issue>: cancel its Multica runs, relabel it stuck, close its PR. Multica owns
// the run's workspace directory and reclaims it (multica daemon disk-usage).
func killCmd() *cobra.Command {
	return &cobra.Command{Use: "kill <issue-number>", Short: "stop one issue's agent session and mark it stuck", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error {
		// n is interpolated into gh searches, so it must be a bare number.
		if _, err := strconv.ParseUint(a[0], 10, 32); err != nil {
			return fmt.Errorf("issue number must be a positive integer, got %q", a[0])
		}
		ctx := context.Background()
		n := a[0]
		R := shell.Quote(cfg.Project.Repo)

		out, err := shell.Output(ctx, fmt.Sprintf("%s issue list --output json --metadata %s --metadata %s", multica,
			shell.Quote(fleetsync.MetaRepo+"="+cfg.Project.Repo), shell.Quote(fleetsync.MetaIssue+"="+n)))
		if err != nil {
			return fmt.Errorf("multica issue list: %w", err)
		}
		rows, _, err := jsonRows(out, "issues")
		if err != nil {
			return err
		}
		for _, r := range rows {
			if id := str(r["id"]); id != "" {
				if err := cancelMultica(ctx, id); err != nil {
					return err
				}
			}
		}
		if len(rows) == 0 && !shell.DryRun {
			fmt.Fprintf(os.Stderr, "no Multica issue mirrors #%s (never dispatched, or already closed)\n", n)
		}
		steps := []string{
			fmt.Sprintf(`gh issue edit -R %s %s --add-label %s --remove-label %s`, R, n, shell.Quote(cfg.Labels.Stuck), shell.Quote(cfg.Labels.Ready)),
			fmt.Sprintf(`gh pr list -R %s --state open --json number,headRefName -q '.[]|select(.headRefName|startswith("agent/%s-"))|.number' | xargs -r -n1 gh pr close -R %s --delete-branch`, R, n, R),
		}
		for _, s := range steps {
			if err := shell.Run(ctx, s, nil); err != nil {
				return err
			}
		}
		return nil
	}}
}

// panic: stop everything, kill agent processes, print the by-hand checklist.
func panicCmd() *cobra.Command {
	return &cobra.Command{Use: "panic", Short: "stop the daemon and sync, kill agents, print the rotation checklist", RunE: func(*cobra.Command, []string) error {
		if err := requireLinux("panic"); err != nil {
			return err
		}
		ctx := context.Background()
		shell.Run(ctx, "systemctl --user stop "+units(), nil)
		shell.Run(ctx, `pkill -x claude; pkill -f "opencode run"; pkill -x agy; true`, nil)
		fmt.Println("NOW, by hand:")
		fmt.Println("  1. GitHub → Settings → Applications → " + cfg.GitHub.AppSlug + " → Suspend installation")
		fmt.Println("  2. Rotate every key in ~/.config/fleet/env and provider dashboards; revoke the Multica PAT (Settings → API Token)")
		fmt.Println("  3. fleet status — confirm no runs alive; inspect open PRs before `fleet resume`")
		return nil
	}}
}
