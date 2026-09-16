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

func Run(ctx context.Context, cmd string, env map[string]string) error {
	fmt.Fprintf(os.Stderr, "→ %s\n", cmd)
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
	fmt.Fprintf(os.Stderr, "→ %s\n", cmd)
	if DryRun {
		return "", nil
	}
	c := exec.CommandContext(ctx, "bash", "-lc", cmd)
	c.Stderr = os.Stderr
	b, err := c.Output()
	return strings.TrimSpace(string(b)), err
}

// Interactive is for commands that need a TTY (OAuth logins).
func Interactive(ctx context.Context, cmd string) error {
	fmt.Fprintf(os.Stderr, "→ (interactive) %s\n", cmd)
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
	fmt.Fprintf(os.Stderr, "→ write %s (%04o, %d bytes)\n", path, perm, len(data))
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
