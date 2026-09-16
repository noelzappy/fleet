package cli

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

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
		&cobra.Command{Use: "update [name]", Args: cobra.MaximumNArgs(1), Short: "update harness CLIs (all, or one) and check min_version", RunE: harnessUpdate},
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
	if err := checkMinVersion(ctx, name, h); err != nil {
		return err
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
	// Refuse before creating anything: a vulnerable CLI must not touch the repo.
	for _, name := range sortedHarnessNames() {
		if err := checkMinVersion(ctx, name, cfg.Harnesses[name]); err != nil {
			return err
		}
	}
	root, wt := shell.Quote(cfg.Project.Root), shell.Quote(cfg.Project.Root+"-wt-verify")
	defer shell.Run(ctx, `cd `+root+` && git worktree remove --force `+wt+`; git branch -D fleet/verify 2>/dev/null; true`, nil)
	if err := shell.Run(ctx, `cd `+root+` && git worktree add `+wt+` -b fleet/verify && cd `+wt+` && pnpm install --frozen-lockfile`, nil); err != nil {
		return err
	}
	prompt := fmt.Sprintf("Run `%s` in this directory. Print exactly PASS or FAIL as the last line.", cfg.Gate.Command)
	gateTimeout, _ := time.ParseDuration(cfg.Gate.Timeout) // validated by config.Load
	agentTimeout := gateTimeout + 10*time.Minute           // the agent reads, runs the gate, and reports
	for _, name := range sortedHarnessNames() {
		h := cfg.Harnesses[name]
		k, err := kindOf(h)
		if err != nil {
			return err
		}
		// pipefail: a failing agent must not be masked by tail's exit status.
		invoke := "set -o pipefail; cd " + wt + " && "
		if h.EnvFile != "" { // e.g. a claude-code harness pointed at another vendor's endpoint
			invoke += ". " + shell.Quote(h.EnvFile) + " && "
		}
		invoke += k.gateRun(prompt, cfg.Gate.Command, agentTimeout) + " | tail -1"
		out, err := shell.Output(ctx, invoke)
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

// harnessUpdate runs each CLI's self-update once (harnesses sharing a binary, like
// claude-code and glm, update together), then checks every harness against min_version.
func harnessUpdate(_ *cobra.Command, args []string) error {
	ctx := context.Background()
	names := sortedHarnessNames()
	if len(args) == 1 {
		if _, ok := cfg.Harnesses[args[0]]; !ok {
			return fmt.Errorf("harness %q not in fleet.yaml", args[0])
		}
		names = []string{args[0]}
	}
	updated := map[string]bool{}
	for _, name := range names {
		h := cfg.Harnesses[name]
		k, err := kindOf(h)
		if err != nil {
			return err
		}
		cmd := k.update
		if h.Update != "" {
			cmd = h.Update
		}
		if updated[cmd] {
			continue
		}
		updated[cmd] = true
		if err := shell.Run(ctx, cmd, nil); err != nil {
			return fmt.Errorf("update %s: %w", name, err)
		}
	}
	var failed []string
	for _, name := range names {
		if err := checkMinVersion(ctx, name, cfg.Harnesses[name]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			failed = append(failed, name)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("below min_version after update: %s", strings.Join(failed, ", "))
	}
	return nil
}

// checkMinVersion fails if the installed CLI is older than the harness's min_version.
func checkMinVersion(ctx context.Context, name string, h config.Harness) error {
	if h.MinVersion == "" {
		return nil
	}
	k, err := kindOf(h)
	if err != nil {
		return err
	}
	out, err := shell.Output(ctx, k.bin+" --version")
	if shell.DryRun {
		fmt.Fprintf(os.Stderr, "  (requires %s >= %s)\n", k.bin, h.MinVersion)
		return nil
	}
	if err != nil {
		return fmt.Errorf("harness %s: %s --version: %w", name, k.bin, err)
	}
	have, err := config.ParseVersion(out)
	if err != nil {
		return fmt.Errorf("harness %s: %w", name, err)
	}
	min, _ := config.ParseVersion(h.MinVersion) // validated by config.Load
	if !config.VersionAtLeast(have, min) {
		return fmt.Errorf("harness %s: %s %s is below min_version %s — run: fleet harness update %s", name, k.bin, versionString(have), h.MinVersion, name)
	}
	return nil
}

func versionString(v []int) string {
	parts := make([]string, len(v))
	for i, n := range v {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ".")
}

func sortedHarnessNames() []string {
	names := make([]string, 0, len(cfg.Harnesses))
	for name := range cfg.Harnesses {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
