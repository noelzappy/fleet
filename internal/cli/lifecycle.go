package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/noelzappy/fleet/internal/shell"
	"github.com/spf13/cobra"
)

// up / pause / resume / kill / panic. Must work from a phone over SSH.

func upCmd() *cobra.Command {
	return &cobra.Command{Use: "up", Short: "start the orchestrator service", RunE: func(*cobra.Command, []string) error {
		if err := requireLinux("up"); err != nil {
			return err
		}
		return shell.Run(context.Background(), `systemctl --user enable --now `+shell.Quote(cfg.Orchestrator.ServiceName), nil)
	}}
}

// pause: soft (default) opens a fleet-paused issue so dispatch stops but in-flight finish; --hard stops the service.
func pauseCmd() *cobra.Command {
	var hard bool
	c := &cobra.Command{Use: "pause", RunE: func(*cobra.Command, []string) error {
		ctx := context.Background()
		if hard {
			if err := requireLinux("pause --hard"); err != nil {
				return err
			}
			return shell.Run(ctx, `systemctl --user stop `+shell.Quote(cfg.Orchestrator.ServiceName), nil)
		}
		body := "paused by fleet cli at " + time.Now().UTC().Format(time.RFC3339)
		return shell.Run(ctx, fmt.Sprintf(`gh issue create -R %s --title "FLEET PAUSED" --label %s --body %s`,
			shell.Quote(cfg.Project.Repo), shell.Quote(cfg.Labels.Paused), shell.Quote(body)), nil)
	}}
	c.Flags().BoolVar(&hard, "hard", false, "stop workers now (worktrees persist)")
	return c
}

func resumeCmd() *cobra.Command {
	return &cobra.Command{Use: "resume", RunE: func(*cobra.Command, []string) error {
		if err := requireLinux("resume"); err != nil {
			return err
		}
		ctx := context.Background()
		nums, err := shell.Output(ctx, fmt.Sprintf(`gh issue list -R %s --label %s --state open --json number -q '.[].number'`, shell.Quote(cfg.Project.Repo), shell.Quote(cfg.Labels.Paused)))
		if err != nil {
			return err
		}
		for _, n := range strings.Fields(nums) {
			shell.Run(ctx, fmt.Sprintf(`gh issue close -R %s %s`, shell.Quote(cfg.Project.Repo), shell.Quote(n)), nil)
		}
		return shell.Run(ctx, `systemctl --user start `+shell.Quote(cfg.Orchestrator.ServiceName), nil)
	}}
}

// kill <issue>: stop that session, relabel stuck, remove worktree, close PR if any.
func killCmd() *cobra.Command {
	return &cobra.Command{Use: "kill <issue-number>", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error {
		ctx := context.Background()
		// n is interpolated into grep patterns and gh searches, so it must be a bare number.
		if _, err := strconv.ParseUint(a[0], 10, 32); err != nil {
			return fmt.Errorf("issue number must be a positive integer, got %q", a[0])
		}
		n := a[0]
		R := shell.Quote(cfg.Project.Repo)
		steps := []string{
			`ao session stop --issue ` + n + ` || true`, // TODO(implementer): confirm ao verb
			fmt.Sprintf(`gh issue edit -R %s %s --add-label %s --remove-label %s`, R, n, shell.Quote(cfg.Labels.Stuck), shell.Quote(cfg.Labels.Ready)),
			fmt.Sprintf(`cd %s && git worktree list | grep -- "-%s-" | awk '{print $1}' | xargs -r -n1 git worktree remove --force`, shell.Quote(cfg.Project.Root), n),
			fmt.Sprintf(`gh pr list -R %s --search "#%s in:title" --json number -q '.[].number' | xargs -r -n1 gh pr close -R %s --delete-branch`, R, n, R),
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
	return &cobra.Command{Use: "panic", Short: "stop service, kill agents, print rotation checklist", RunE: func(*cobra.Command, []string) error {
		if err := requireLinux("panic"); err != nil {
			return err
		}
		ctx := context.Background()
		shell.Run(ctx, `systemctl --user stop `+shell.Quote(cfg.Orchestrator.ServiceName), nil)
		shell.Run(ctx, `pkill -x claude; pkill -f "opencode run"; pkill -x agy; true`, nil)
		fmt.Println("NOW, by hand:")
		fmt.Println("  1. GitHub → Settings → Applications → " + cfg.GitHub.AppSlug + " → Suspend installation")
		fmt.Println("  2. Rotate every key in ~/.config/fleet/env and provider dashboards")
		fmt.Println("  3. fleet status — confirm no sessions alive; inspect open PRs before resuming")
		return nil
	}}
}
