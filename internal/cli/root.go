package cli

import (
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/noelzappy/fleet/internal/config"
	"github.com/noelzappy/fleet/internal/ghapp"
	"github.com/noelzappy/fleet/internal/platform"
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
			if cfg, err = config.Load(cfgPath); err != nil {
				return err
			}
			if projectScoped() {
				shell.PathPrefix = ghapp.WrapperDir
			}
			return nil
		},
	}
	root.PersistentFlags().StringVarP(&cfgPath, "config", "c", "fleet.yaml", "path to fleet.yaml")
	root.PersistentFlags().BoolVar(&shell.DryRun, "dry-run", false, "print commands without running them")
	root.AddCommand(
		initCmd(), bootstrapCmd(), harnessCmd(), githubCmd(), orchestratorCmd(),
		issuesCmd(), syncCmd(), upCmd(), pauseCmd(), resumeCmd(), killCmd(), panicCmd(),
		statusCmd(), digestCmd(), versionCmd(),
	)
	return root
}

// requireBox guards commands that act on the fleet box (package managers, the
// service manager, firewall) and returns its platform. Under --dry-run any OS may
// preview any platform: FLEET_PLATFORM=linux|darwin picks which.
func requireBox(name string) (platform.Platform, error) {
	goos := runtime.GOOS
	if shell.DryRun {
		if o := os.Getenv("FLEET_PLATFORM"); o != "" {
			goos = o
		}
	}
	p, err := platform.For(goos)
	if err != nil {
		return nil, fmt.Errorf("%s acts on the fleet box: %w (or preview with --dry-run)", name, err)
	}
	return p, nil
}

// box is requireBox for commands that only read service state and never fail on
// an unsupported OS (status): it falls back to Linux for the command shapes.
func box() platform.Platform {
	p, err := requireBox("")
	if err != nil {
		return platform.Linux{}
	}
	return p
}

// version is stamped at release time: -ldflags "-X github.com/noelzappy/fleet/internal/cli.version=v0.1.0".
var version = "0.1.0-dev"

func versionCmd() *cobra.Command {
	return &cobra.Command{Use: "version", Run: func(c *cobra.Command, _ []string) { c.Println("fleet " + strings.TrimPrefix(version, "v")) }}
}
