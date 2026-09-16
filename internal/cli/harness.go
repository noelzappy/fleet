package cli

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/noelzappy/fleet/internal/config"
	"github.com/noelzappy/fleet/internal/shell"
	"github.com/spf13/cobra"
)

// fleet harness add|login|verify — install, authenticate and prove each agent CLI runs headless.
func harnessCmd() *cobra.Command {
	c := &cobra.Command{Use: "harness", Short: "Install, authenticate, and smoke-test agent harnesses"}
	c.AddCommand(
		&cobra.Command{Use: "add <name>", Args: cobra.ExactArgs(1), Short: "install + write env profile + smoke test", RunE: harnessAdd},
		&cobra.Command{Use: "login <name>", Args: cobra.ExactArgs(1), Short: "interactive OAuth login (run inside tmux over SSH)", RunE: harnessLogin},
		&cobra.Command{Use: "verify", Short: "run the gate from every harness in a throwaway worktree", RunE: harnessVerify},
	)
	return c
}

func harnessAdd(cmd *cobra.Command, args []string) error {
	name := args[0]
	h, ok := cfg.Harnesses[name]
	if !ok {
		return fmt.Errorf("harness %q not in fleet.yaml", name)
	}
	ctx := context.Background()
	env, err := expandHarnessEnv(h.Env)
	if err != nil {
		return fmt.Errorf("harness %s: %w", name, err)
	}
	if h.Install != "" {
		if err := shell.Run(ctx, h.Install, nil); err != nil {
			return err
		}
	}
	if len(env) > 0 && h.EnvFile != "" {
		path := h.EnvFile // ~ expanded by config.ApplyDefaults
		var b strings.Builder
		for k, v := range env {
			fmt.Fprintf(&b, "export %s=%s\n", k, shell.Quote(v))
		}
		if err := shell.WriteFile(path, []byte(b.String()), 0o600); err != nil {
			return err
		}
	}
	if h.Smoke != "" {
		if err := shell.Run(ctx, h.Smoke, env); err != nil {
			return fmt.Errorf("smoke failed for %s — if login is needed: fleet harness login %s", name, name)
		}
	}
	return nil
}

func harnessLogin(cmd *cobra.Command, args []string) error {
	h, ok := cfg.Harnesses[args[0]]
	if !ok || h.Login == "" {
		return fmt.Errorf("no login command for %s", args[0])
	}
	return shell.Interactive(context.Background(), h.Login)
}

// harnessVerify: worktree → pnpm install → each harness runs the gate headless.
// The step that proves a harness can actually do the job (docker, network, sandbox).
func harnessVerify(cmd *cobra.Command, _ []string) error {
	ctx := context.Background()
	root, wt := shell.Quote(cfg.Project.Root), shell.Quote(cfg.Project.Root+"-wt-verify")
	defer shell.Run(ctx, `cd `+root+` && git worktree remove --force `+wt+`; git branch -D fleet/verify 2>/dev/null; true`, nil)
	if err := shell.Run(ctx, `cd `+root+` && git worktree add `+wt+` -b fleet/verify && cd `+wt+` && pnpm install --frozen-lockfile`, nil); err != nil {
		return err
	}
	prompt := shell.Quote(fmt.Sprintf("Run `%s` in this directory. Print exactly PASS or FAIL as the last line.", cfg.Gate.Command))
	names := make([]string, 0, len(cfg.Harnesses))
	for name := range cfg.Harnesses {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		h := cfg.Harnesses[name]
		var invoke string
		switch h.Kind {
		case "claude-code":
			invoke = fmt.Sprintf(`cd %s && claude -p %s --output-format text`, wt, prompt)
		case "opencode":
			invoke = fmt.Sprintf(`cd %s && opencode run %s`, wt, prompt)
		case "gemini-cli":
			invoke = fmt.Sprintf(`cd %s && gemini -p %s`, wt, prompt)
		default:
			fmt.Fprintf(os.Stderr, "verify %s: unknown kind %s\n", name, h.Kind)
			continue
		}
		// TODO(implementer): confirm each CLI's headless flags on this box; capture output and parse last line.
		out, err := shell.Output(ctx, invoke+` | tail -1`)
		status := "FAIL"
		switch {
		case shell.DryRun:
			status = "(dry-run)"
		case err == nil && strings.Contains(out, "PASS"):
			status = "PASS"
		}
		fmt.Printf("%-14s %-12s %s\n", name, h.Kind, status)
	}
	return nil
}

// expandHarnessEnv resolves ${VAR} references against the secrets file and process env.
// Under --dry-run a missing secret is a warning, so previews work on a laptop without secrets.
func expandHarnessEnv(m map[string]string) (map[string]string, error) {
	lookup, err := config.SecretsLookup()
	if err != nil {
		return nil, err
	}
	env, err := config.ExpandEnv(m, lookup)
	if err != nil && shell.DryRun {
		fmt.Fprintln(os.Stderr, "warning:", err)
		return m, nil
	}
	return env, err
}
