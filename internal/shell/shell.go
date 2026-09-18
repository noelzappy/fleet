// Package shell runs every external command and file write, with dry-run and streaming.
// Every subcommand shells out through here so `fleet --dry-run` prints exactly what
// would run. Keep this the only place os/exec is used.
package shell

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var DryRun bool

// Quiet suppresses the "→ command" trace on stderr. `fleet github token` sets it: git and
// every gh call through the wrapper run it, and the trace would land in agent output.
var Quiet bool

func trace(format string, a ...any) {
	if !Quiet {
		fmt.Fprint(os.Stderr, Redact(fmt.Sprintf(format, a...)))
	}
}

var (
	bearer = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]+`)
	// Multica PATs (mul_, mcn_) and GitHub tokens; anything else secret should travel
	// through a 0600 file or stdin, not the command line.
	tokenLike = regexp.MustCompile(`\b(mul|mcn|ghp|gho|ghs|ghu|github_pat)_[A-Za-z0-9_]{8,}`)
)

// Redact masks credentials in a command trace, which lands in terminals, scrollback,
// systemd logs and pasted bug reports.
func Redact(s string) string {
	s = bearer.ReplaceAllString(s, "${1}***")
	return tokenLike.ReplaceAllString(s, "${1}_***")
}

func Run(ctx context.Context, cmd string, env map[string]string) error {
	trace("→ %s\n", cmd)
	if DryRun {
		return nil
	}
	c := exec.CommandContext(ctx, "bash", "-lc", cmd)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	c.Env = os.Environ()
	for k, v := range env {
		c.Env = append(c.Env, k+"="+v) // already expanded by config.ExpandEnv
	}
	return c.Run()
}

func Output(ctx context.Context, cmd string) (string, error) {
	trace("→ %s\n", cmd)
	if DryRun {
		return "", nil
	}
	c := exec.CommandContext(ctx, "bash", "-lc", cmd)
	c.Stderr = os.Stderr
	b, err := c.Output()
	return strings.TrimSpace(string(b)), err
}

// OutputInput is Output with stdin supplied, for commands that read a body from
// stdin (issue descriptions, comments) so it never touches the argv or shell history.
func OutputInput(ctx context.Context, cmd, stdin string) (string, error) {
	trace("→ %s  <<(%d bytes on stdin)\n", cmd, len(stdin))
	if DryRun {
		return "", nil
	}
	c := exec.CommandContext(ctx, "bash", "-lc", cmd)
	c.Stdin = strings.NewReader(stdin)
	c.Stderr = os.Stderr
	b, err := c.Output()
	return strings.TrimSpace(string(b)), err
}

// Check runs cmd quietly and reports whether it exited 0. It is how idempotent steps
// decide whether to act. Under --dry-run it prints cmd and reports false, so the
// action that would follow is printed too.
func Check(ctx context.Context, cmd string) bool {
	if DryRun {
		trace("→ (check) %s\n", cmd)
		return false
	}
	return exec.CommandContext(ctx, "bash", "-lc", cmd).Run() == nil
}

// Interactive is for commands that need a TTY (OAuth logins).
func Interactive(ctx context.Context, cmd string) error {
	trace("→ (interactive) %s\n", cmd)
	if DryRun {
		return nil
	}
	c := exec.CommandContext(ctx, "bash", "-lc", cmd)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

// WriteFile is the only way the CLI writes files, so --dry-run covers writes as well as
// commands. Parent directories are created.
func WriteFile(path string, data []byte, perm os.FileMode) error {
	trace("→ write %s (%04o, %d bytes)\n", path, perm, len(data))
	if DryRun {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, perm); err != nil {
		return err
	}
	return os.Chmod(path, perm) // WriteFile keeps the old mode on an existing file
}

var safeWord = regexp.MustCompile(`^[A-Za-z0-9_./:@%+=,-]+$`)

// Quote makes s a single literal bash word. Use it for every value interpolated into a
// command string: Go's %q is Go syntax, and bash still expands $, backticks and \ inside it.
// Plain words are left bare so --dry-run output stays readable.
func Quote(s string) string {
	if safeWord.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
