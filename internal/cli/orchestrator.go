package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/noelzappy/fleet/internal/shell"
	"github.com/noelzappy/fleet/internal/templates"
	"github.com/spf13/cobra"
)

// fleet orchestrator init|run — generate config from fleet.yaml; run it (systemd calls run).
func orchestratorCmd() *cobra.Command {
	c := &cobra.Command{Use: "orchestrator", Short: "Generate config for and run the underlying orchestrator (ao | vibe-kanban)"}
	c.AddCommand(
		&cobra.Command{Use: "init", RunE: orchInit},
		&cobra.Command{Use: "run", Short: "foreground; systemd calls this", RunE: orchRun},
	)
	return c
}

func orchInit(cmd *cobra.Command, _ []string) error {
	if err := requireLinux("orchestrator init"); err != nil {
		return err
	}
	ctx := context.Background()
	switch cfg.Orchestrator.Kind {
	case "ao":
		if err := shell.Run(ctx, `command -v ao >/dev/null || npm i -g @aoagents/ao`, nil); err != nil {
			return err
		}
		b, err := templates.Render("agent-orchestrator.yaml.tmpl", cfg)
		if err != nil {
			return err
		}
		dst := filepath.Join(cfg.Project.Root, cfg.Orchestrator.ConfigPath)
		if err := shell.WriteFile(dst, b, 0o644); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "VERIFY keys in", dst, "against `ao config-help` before first run")
	default:
		return fmt.Errorf("orchestrator kind %q: TODO(implementer)", cfg.Orchestrator.Kind)
	}
	u, err := templates.Render("fleet.service.tmpl", cfg)
	if err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	unit := filepath.Join(home, ".config/systemd/user", cfg.Orchestrator.ServiceName+".service")
	if err := shell.WriteFile(unit, u, 0o644); err != nil {
		return err
	}
	return shell.Run(ctx, `systemctl --user daemon-reload && loginctl enable-linger $USER`, nil)
}

func orchRun(cmd *cobra.Command, _ []string) error {
	if err := requireLinux("orchestrator run"); err != nil {
		return err
	}
	switch cfg.Orchestrator.Kind {
	case "ao":
		// TODO(implementer): confirm flags on the installed ao version; dashboard must bind to the tailscale IP only.
		return shell.Run(context.Background(), `cd `+shell.Quote(cfg.Project.Root)+` && ao start --config `+shell.Quote(cfg.Orchestrator.ConfigPath)+` --bind `+shell.Quote(cfg.Orchestrator.DashboardBind), nil)
	}
	return fmt.Errorf("unsupported orchestrator %q", cfg.Orchestrator.Kind)
}
