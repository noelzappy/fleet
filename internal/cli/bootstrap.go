package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/noelzappy/fleet/internal/platform"
	"github.com/noelzappy/fleet/internal/shell"
	"github.com/noelzappy/fleet/internal/ui"
	"github.com/spf13/cobra"
)

// fleet bootstrap — prepare the box (Ubuntu or macOS). Idempotent: every step
// checks before acting. The steps themselves live in internal/platform.
func bootstrapCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "bootstrap",
		Short: "Install OS deps, docker, node/pnpm, gh, tailscale; lock down the box; create dirs",
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := requireBox("bootstrap")
			if err != nil {
				return err
			}
			if os.Geteuid() == 0 && !shell.DryRun {
				return fmt.Errorf("run bootstrap as the non-root user the fleet will run as, not root — see README › Requirements")
			}
			changed, err := runSteps(context.Background(), p.BootstrapSteps(cfg.Machine, cfg.Project.Root))
			if err != nil {
				return err
			}
			if changed == 0 && !shell.DryRun {
				cmd.Println("bootstrap: nothing to do")
			}
			for _, n := range p.Notes(cfg.Machine) {
				cmd.Println("NOTE:", n)
			}
			cmd.Println("Next: fleet harness add <name> for each harness in fleet.yaml")
			return nil
		},
	}
}

// runSteps applies each step whose check fails and returns how many it changed.
// The check runs again after apply, so a step that "succeeds" without taking
// effect (a binary not on PATH, a config that's overridden) fails loudly.
func runSteps(ctx context.Context, steps []platform.Step) (changed int, err error) {
	for _, s := range steps {
		if shell.Check(ctx, s.Check) {
			ui.Errf("✓ %s\n", s.Name)
			continue
		}
		ui.Errf("● %s\n", s.Name)
		if err := shell.Run(ctx, s.Apply, nil); err != nil {
			return changed, fmt.Errorf("%s: %w", s.Name, err)
		}
		changed++
		if !shell.DryRun && !shell.Check(ctx, s.Check) {
			return changed, fmt.Errorf("%s: applied, but the check still fails: %s", s.Name, s.Check)
		}
	}
	return changed, nil
}
