package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/noelzappy/fleet/internal/config"
	"github.com/noelzappy/fleet/internal/shell"
)

// harnessKind is everything fleet knows about one agent CLI. Each entry was checked
// against the installed binary's --help; update it when the CLI changes.
type harnessKind struct {
	bin    string
	update string // self-update command; a harness's `update:` overrides it
	// auth exits 0 iff the CLI is signed in. It must be cheap and never prompt:
	// sync runs it for every harness on every tick.
	auth string
	// gateRun returns a non-interactive invocation whose only shell permission is the
	// gate command. timeout bounds the whole agent run.
	gateRun func(prompt, gate string, timeout time.Duration) string
}

var harnessKinds = map[string]harnessKind{
	// claude 2.1.x: -p/--print is non-interactive. --allowedTools scopes Bash to the gate;
	// anything else needing approval is denied rather than prompted.
	"claude-code": {
		bin:    "claude",
		update: "claude update",
		// `claude auth status` prints JSON with "loggedIn".
		auth: `claude auth status 2>/dev/null | grep -Eq '"loggedIn": *true'`,
		gateRun: func(p, gate string, _ time.Duration) string {
			return "claude -p " + shell.Quote(p) + " --output-format text --allowedTools " +
				shell.Quote("Bash("+gate+")") + " " + shell.Quote("Bash("+gate+" *)")
		},
	},
	// opencode 1.18.x: `run` is non-interactive. OPENCODE_CONFIG_CONTENT layers an inline
	// permission config: the gate is allowed, every other shell command and all edits denied.
	"opencode": {
		bin:    "opencode",
		update: "opencode upgrade",
		// `opencode auth list` ends with "N credentials"; env-var providers need auth_check.
		auth: `opencode auth list 2>&1 | grep -Eq '(^|[^0-9])[1-9][0-9]* credentials'`,
		gateRun: func(p, gate string, _ time.Duration) string {
			perm, _ := json.Marshal(map[string]any{"permission": map[string]any{
				"edit": "deny", "webfetch": "deny",
				"bash": map[string]string{"*": "deny", gate: "allow", gate + " *": "allow"},
			}})
			return "OPENCODE_CONFIG_CONTENT=" + shell.Quote(string(perm)) + " opencode run " + shell.Quote(p)
		},
	},
	// agy (Antigravity CLI, which replaced Gemini CLI) 1.1.x: -p/--print is non-interactive
	// and --print-timeout defaults to 5m, shorter than most gates.
	// LIMITATION: agy has no per-command allowlist flag, only --dangerously-skip-permissions,
	// so an antigravity harness gets every tool during verify. Run it only on a box whose
	// secrets you'd rotate after `fleet panic` anyway.
	"antigravity": {
		bin:    "agy",
		update: "agy update",
		// agy has no status command: signed in = an OAuth token file exists (never read),
		// or an API key is configured.
		auth: `test -s "$HOME/.gemini/antigravity-cli/antigravity-oauth-token" || test -n "${GEMINI_API_KEY:-}"`,
		gateRun: func(p, _ string, t time.Duration) string {
			return "agy -p " + shell.Quote(p) + " --output-format text --dangerously-skip-permissions --print-timeout " + t.String()
		},
	},
	// codex (OpenAI Codex CLI) 0.15x: `exec` is non-interactive and prints only the final
	// message on stdout. No per-command allowlist, and --sandbox workspace-write blocks the
	// network real gates need, so verify bypasses approvals and the sandbox (as Multica does
	// in production). --ephemeral keeps verify runs out of Codex's session history.
	"codex": {
		bin:    "codex",
		update: "codex update",
		// `codex login status` exits 1 when signed out.
		auth: "codex login status >/dev/null 2>&1",
		gateRun: func(p, _ string, _ time.Duration) string {
			return "codex exec --dangerously-bypass-approvals-and-sandbox --skip-git-repo-check --ephemeral " + shell.Quote(p)
		},
	},
}

func kindOf(h config.Harness) (harnessKind, error) {
	k, ok := harnessKinds[h.Kind]
	if !ok {
		return harnessKind{}, fmt.Errorf("unknown harness kind %q", h.Kind)
	}
	return k, nil
}

// authCheckFor returns the command that exits 0 iff the harness is signed in.
func authCheckFor(h config.Harness) string {
	if h.AuthCheck != "" {
		return h.AuthCheck
	}
	if k, ok := harnessKinds[h.Kind]; ok {
		return k.auth
	}
	return "false"
}

// signedOut runs every harness's auth check once (harnesses sharing a check share the
// result) and returns the names of harnesses that aren't signed in. Under --dry-run
// nothing runs and every harness counts as signed in.
func signedOut(ctx context.Context, harnesses map[string]config.Harness) map[string]bool {
	out := map[string]bool{}
	byCheck := map[string]bool{}
	names := make([]string, 0, len(harnesses))
	for n := range harnesses {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		check := authCheckFor(harnesses[n])
		ok, seen := byCheck[check]
		if !seen {
			ok = shell.DryRun || shell.Check(ctx, check)
			byCheck[check] = ok
		}
		if !ok {
			out[n] = true
		}
	}
	return out
}
