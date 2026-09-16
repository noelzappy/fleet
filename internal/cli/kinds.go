package cli

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/noelzappy/fleet/internal/config"
	"github.com/noelzappy/fleet/internal/shell"
)

// harnessKind is everything fleet knows about one agent CLI. Each entry was checked
// against the installed binary's --help; update it when the CLI changes.
type harnessKind struct {
	bin    string
	update string // self-update command; a harness's `update:` overrides it
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
		gateRun: func(p, _ string, t time.Duration) string {
			return "agy -p " + shell.Quote(p) + " --output-format text --dangerously-skip-permissions --print-timeout " + t.String()
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
