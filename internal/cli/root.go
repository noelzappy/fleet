package cli

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/noelzappy/fleet/internal/config"
	"github.com/noelzappy/fleet/internal/shell"
	"github.com/spf13/cobra"
)

var (
	cfgPath string
	cfg     *config.Fleet
)

func Root() *cobra.Command {
	root := &cobra.Command{
		Use:          "fleet",
		Short:        "Set up, run, and supervise a multi-model coding-agent fleet against a repo",
		SilenceUsage: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			// Match the full path: `orchestrator init` and `github init` are also named "init".
			switch p := cmd.CommandPath(); {
			case p == "fleet init", p == "fleet version", p == "fleet help", strings.HasPrefix(p, "fleet completion"):
				return nil
			}
			var err error
			cfg, err = config.Load(cfgPath)
			return err
		},
	}
	root.PersistentFlags().StringVarP(&cfgPath, "config", "c", "fleet.yaml", "path to fleet.yaml")
	root.PersistentFlags().BoolVar(&shell.DryRun, "dry-run", false, "print commands without running them")
	root.AddCommand(
		initCmd(), bootstrapCmd(), harnessCmd(), githubCmd(), orchestratorCmd(),
		issuesCmd(), upCmd(), pauseCmd(), resumeCmd(), killCmd(), panicCmd(),
		statusCmd(), digestCmd(), versionCmd(),
	)
	return root
}

// requireLinux guards commands that act on the fleet box (systemd, apt, ufw).
// --dry-run is exempt so every command can be previewed from a laptop.
func requireLinux(name string) error {
	if runtime.GOOS != "linux" && !shell.DryRun {
		return fmt.Errorf("%s acts on the fleet box — run this on the fleet box (or preview with --dry-run)", name)
	}
	return nil
}

func versionCmd() *cobra.Command {
	return &cobra.Command{Use: "version", Run: func(c *cobra.Command, _ []string) { c.Println("fleet 0.1.0-dev") }}
}
