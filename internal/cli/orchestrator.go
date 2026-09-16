package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/noelzappy/fleet/internal/config"
	"github.com/noelzappy/fleet/internal/shell"
	"github.com/noelzappy/fleet/internal/templates"
	"github.com/spf13/cobra"
)

// fleet orchestrator init|run — stand up Multica from fleet.yaml; run its daemon.
// Multica is the execution backend (docs/orchestrator-decision.md): a web UI + API in
// docker compose, and a daemon on this box that runs the agent CLIs. fleet creates the
// workspace, repo and one Multica agent per profile, and installs the systemd units
// for the daemon and for `fleet sync`.
func orchestratorCmd() *cobra.Command {
	c := &cobra.Command{Use: "orchestrator", Short: "Set up and run Multica from fleet.yaml"}
	c.AddCommand(
		&cobra.Command{Use: "init", Short: "install Multica, create workspace/repo/agents, install systemd units", RunE: orchInit},
		&cobra.Command{Use: "run", Short: "run the Multica daemon in the foreground; the systemd unit calls this", RunE: orchRun},
	)
	return c
}

// providerFor maps a harness kind to the Multica runtime provider that drives it
// (names from multica/server/pkg/agent/agent.go).
var providerFor = map[string]string{"claude-code": "claude", "opencode": "opencode", "antigravity": "antigravity"}

const multicaRepo = "https://github.com/multica-ai/multica.git"

type orchData struct {
	*config.Fleet
	ConfigPath string // absolute fleet.yaml path baked into the units
	Bind       string
	JWT        string
	PGPass     string
	VCSKey     string
	OwnerEmail string
}

func orchInit(cmd *cobra.Command, _ []string) error {
	if err := requireLinux("orchestrator init"); err != nil {
		return err
	}
	ctx := context.Background()
	O := cfg.Orchestrator
	dir := shell.Quote(O.Dir)
	bind := O.DashboardBind
	if bind == "" || strings.Contains(bind, ":") || bind == "0.0.0.0" {
		return fmt.Errorf("orchestrator.dashboard_bind must be an IP (your tailscale IP), got %q", bind)
	}
	compose := "docker compose -f docker-compose.selfhost.yml -f docker-compose.fleet.yml"

	secret := func(name, gen string) step {
		return step{
			name:  "secret " + name,
			check: "grep -q '^" + name + "=' " + config.SecretsFile,
			apply: "echo " + name + "=$(" + gen + ") >> " + config.SecretsFile,
		}
	}
	if _, err := runSteps(ctx, []step{
		{name: "multica CLI", check: "command -v multica >/dev/null",
			apply: "curl -fsSL https://raw.githubusercontent.com/multica-ai/multica/main/scripts/install.sh | bash"},
		{name: "multica checkout (latest release tag)", check: "test -d " + dir + "/.git",
			apply: "git clone --depth 1 " + multicaRepo + " " + dir + " && cd " + dir + " && git fetch --tags --depth 1 && git checkout -q $(git tag -l 'v*' --sort=-v:refname | head -1)"},
		secret("JWT_SECRET", "openssl rand -hex 32"),
		secret("POSTGRES_PASSWORD", "openssl rand -hex 24"),
		secret("MULTICA_VCS_SECRET_KEY", "openssl rand -base64 32"),
	}); err != nil {
		return err
	}

	// Compose env and override, rendered from the secrets file so nothing secret is in Go or git.
	data, err := orchTemplateData(bind)
	if err != nil {
		return err
	}
	for _, f := range []struct {
		tmpl, dst string
		mode      os.FileMode
	}{
		{"multica.env.tmpl", filepath.Join(O.Dir, ".env"), 0o600},
		{"docker-compose.fleet.yml.tmpl", filepath.Join(O.Dir, "docker-compose.fleet.yml"), 0o644},
		{"fleet-multica.service.tmpl", unitPath(O.ServiceName + ".service"), 0o644},
		{"fleet-multica-sync.service.tmpl", unitPath(O.ServiceName + "-sync.service"), 0o644},
		{"fleet-multica-sync.timer.tmpl", unitPath(O.ServiceName + "-sync.timer"), 0o644},
	} {
		b, err := templates.Render(f.tmpl, data)
		if err != nil {
			return err
		}
		if err := shell.WriteFile(f.dst, b, f.mode); err != nil {
			return err
		}
	}

	api := "http://" + bind + ":8080"
	app := "http://" + bind + ":3000"
	if _, err := runSteps(ctx, []step{
		{name: "multica server up (" + app + ")", check: "curl -fsS " + api + "/readyz >/dev/null 2>&1",
			apply: "cd " + dir + " && " + compose + " up -d && for i in $(seq 1 90); do curl -fsS " + api + "/readyz >/dev/null 2>&1 && exit 0; sleep 2; done; echo 'multica did not become ready' >&2; exit 1"},
		{name: "multica CLI points at this server", check: "multica config show 2>/dev/null | grep -q " + shell.Quote(api),
			apply: "multica setup self-host --server-url " + api + " --app-url " + app},
		{name: "multica login", check: "multica auth status >/dev/null 2>&1",
			apply: fmt.Sprintf(`echo "Open %s over Tailscale and sign up. With no mail server the sign-in code is in the backend log:"; echo "  cd %s && %s logs backend | grep -i code"; echo "Then create a token under Settings → API Token and paste it below."; multica login --token`, app, dir, compose)},
		{name: "systemd units loaded", check: "systemctl --user cat " + O.ServiceName + " >/dev/null 2>&1 && systemctl --user cat " + O.ServiceName + "-sync.timer >/dev/null 2>&1",
			apply: "systemctl --user daemon-reload && loginctl enable-linger $USER"},
	}); err != nil {
		return err
	}

	if err := ensureWorkspace(ctx); err != nil {
		return err
	}
	if _, err := runSteps(ctx, []step{
		{name: "repo registered", check: "multica repo list --output json | grep -q " + shell.Quote(cfg.Project.Repo),
			apply: "multica repo add https://github.com/" + shell.Quote(cfg.Project.Repo)},
		{name: "daemon running", check: "systemctl --user is-active --quiet " + O.ServiceName,
			apply: "systemctl --user enable --now " + O.ServiceName},
	}); err != nil {
		return err
	}
	runtimes, err := waitRuntimes(ctx)
	if err != nil {
		return err
	}
	if err := ensureAgents(ctx, runtimes); err != nil {
		return err
	}
	if _, err := runSteps(ctx, []step{
		{name: "sync timer running", check: "systemctl --user is-active --quiet " + O.ServiceName + "-sync.timer",
			apply: "systemctl --user enable --now " + O.ServiceName + "-sync.timer"},
	}); err != nil {
		return err
	}
	cmd.Printf("Multica is up: %s (over Tailscale). Next: fleet github init, fleet issues sync <file>, fleet status\n", app)
	return nil
}

// orchRun is what the systemd unit executes: the Multica daemon in the foreground,
// with the flags derived from fleet.yaml kept here rather than in the unit file.
func orchRun(cmd *cobra.Command, _ []string) error {
	if err := requireLinux("orchestrator run"); err != nil {
		return err
	}
	total := 0
	for _, p := range cfg.Profiles {
		total += p.Concurrency
	}
	if total == 0 {
		total = 1
	}
	return shell.Run(context.Background(), fmt.Sprintf(
		"exec multica daemon start --foreground --runtime-name fleet --no-auto-update --max-concurrent-tasks %d --workspaces-root %s",
		total, shell.Quote(cfg.Project.Root+"-runs")), nil)
}

func orchTemplateData(bind string) (orchData, error) {
	lookup, err := config.SecretsLookup()
	if err != nil {
		return orchData{}, err
	}
	get := func(name string) string {
		v, ok := lookup(name)
		if !ok {
			if shell.DryRun {
				return "${" + name + "}"
			}
			v = ""
		}
		return v
	}
	abs, err := filepath.Abs(cfgPath)
	if err != nil {
		return orchData{}, err
	}
	d := orchData{Fleet: cfg, ConfigPath: abs, Bind: bind, OwnerEmail: cfg.Orchestrator.OwnerEmail,
		JWT: get("JWT_SECRET"), PGPass: get("POSTGRES_PASSWORD"), VCSKey: get("MULTICA_VCS_SECRET_KEY")}
	if !shell.DryRun && (d.JWT == "" || d.PGPass == "" || d.VCSKey == "") {
		return d, fmt.Errorf("JWT_SECRET, POSTGRES_PASSWORD and MULTICA_VCS_SECRET_KEY must be in %s (the secret steps above write them)", config.SecretsFile)
	}
	return d, nil
}

func unitPath(name string) string {
	return config.ExpandPath(filepath.Join("~/.config/systemd/user", name))
}

// ensureWorkspace creates the Multica workspace named in fleet.yaml if missing and
// makes it the CLI's default, so every later multica call lands in it.
func ensureWorkspace(ctx context.Context) error {
	name := cfg.Orchestrator.Workspace
	out, err := shell.Output(ctx, "multica workspace list --output json")
	if err != nil {
		return fmt.Errorf("multica workspace list: %w", err)
	}
	rows, _, err := jsonRows(out, "workspaces")
	if err != nil {
		return err
	}
	id := ""
	for _, r := range rows {
		if str(r["name"]) == name || str(r["slug"]) == name {
			id = str(r["id"])
		}
	}
	if id == "" {
		fmt.Fprintf(os.Stderr, "● workspace %s\n", name)
		out, err := shell.Output(ctx, fmt.Sprintf("multica workspace create --output json --name %s --slug %s --issue-prefix %s",
			shell.Quote(name), shell.Quote(slug(name)), shell.Quote(issuePrefix(name))))
		if err != nil {
			return fmt.Errorf("multica workspace create: %w", err)
		}
		var row map[string]any
		if err := decode(out, &row); err != nil {
			return err
		}
		id = str(row["id"])
		if id == "" && !shell.DryRun {
			return fmt.Errorf("multica workspace create: no id in %q", out)
		}
	} else {
		fmt.Fprintf(os.Stderr, "✓ workspace %s\n", name)
	}
	if id == "" {
		id = "<workspace-id>"
	}
	return shell.Run(ctx, "multica workspace switch "+shell.Quote(id), nil)
}

func issuePrefix(name string) string {
	p := strings.ToUpper(slugRE.ReplaceAllString(strings.ToLower(name), ""))
	if len(p) > 4 {
		p = p[:4]
	}
	if p == "" {
		p = "FLT"
	}
	return p
}

// waitRuntimes returns provider → runtime id for the daemon's online runtimes. The
// daemon registers one runtime per agent CLI it finds, shortly after it starts.
func waitRuntimes(ctx context.Context) (map[string]string, error) {
	need := map[string]bool{}
	for _, h := range cfg.Harnesses {
		need[providerFor[h.Kind]] = true
	}
	for attempt := 0; ; attempt++ {
		out, err := shell.Output(ctx, "multica runtime list --output json")
		if err != nil {
			return nil, fmt.Errorf("multica runtime list: %w", err)
		}
		rows, _, err := jsonRows(out, "runtimes")
		if err != nil {
			return nil, err
		}
		got := map[string]string{}
		for _, r := range rows {
			if p := str(r["provider"]); need[p] && str(r["status"]) != "offline" {
				got[p] = str(r["id"])
			}
		}
		var missing []string
		for p := range need {
			if got[p] == "" {
				missing = append(missing, p)
			}
		}
		sort.Strings(missing)
		if len(missing) == 0 {
			return got, nil
		}
		if shell.DryRun {
			for _, p := range missing {
				got[p] = "<runtime-" + p + ">"
			}
			return got, nil
		}
		if attempt >= 30 {
			return nil, fmt.Errorf("no online Multica runtime for %s after 60s — is the CLI installed and logged in on this box? (journalctl --user -u %s)", strings.Join(missing, ", "), cfg.Orchestrator.ServiceName)
		}
		time.Sleep(2 * time.Second)
	}
}

// ensureAgents creates one Multica agent per profile: profile name, model, concurrency,
// the harness's env, and instructions pointing at AGENTS.md. Existing agents are left
// alone (change them with `multica agent update`).
func ensureAgents(ctx context.Context, runtimes map[string]string) error {
	out, err := shell.Output(ctx, "multica agent list --output json")
	if err != nil {
		return fmt.Errorf("multica agent list: %w", err)
	}
	rows, _, err := jsonRows(out, "agents")
	if err != nil {
		return err
	}
	exists := map[string]bool{}
	for _, r := range rows {
		exists[str(r["name"])] = true
	}
	for _, name := range cfg.ProfilesWhere(func(string, config.Profile) bool { return true }) {
		if exists[name] {
			fmt.Fprintf(os.Stderr, "✓ agent %s\n", name)
			continue
		}
		p := cfg.Profiles[name]
		h := cfg.Harnesses[p.Harness]
		fmt.Fprintf(os.Stderr, "● agent %s\n", name)
		conc := p.Concurrency
		if conc < 1 {
			conc = 1 // Multica's minimum; sync never routes to a concurrency-0 profile anyway
		}
		instructions := fmt.Sprintf("You are fleet profile %s: %s for %s, vendor %s. Every issue you get mirrors a GitHub issue; its description says which. Follow AGENTS.md in the repository exactly. Use `gh` for everything on GitHub.", name, p.Role, cfg.Project.Repo, p.Vendor)
		cmd := fmt.Sprintf("multica agent create --output json --name %s --runtime-id %s --max-concurrent-tasks %d --instructions %s",
			shell.Quote(name), shell.Quote(runtimes[providerFor[h.Kind]]), conc, shell.Quote(instructions))
		if p.Model != "" {
			cmd += " --model " + shell.Quote(p.Model)
		}
		env, err := expandHarnessEnv(mergeEnv(h.Env, p.Env))
		if err != nil {
			return fmt.Errorf("profile %s: %w", name, err)
		}
		if len(env) > 0 {
			b, _ := json.Marshal(env)
			if _, err := shell.OutputInput(ctx, cmd+" --custom-env-stdin", string(b)); err != nil {
				return fmt.Errorf("multica agent create %s: %w", name, err)
			}
			continue
		}
		if _, err := shell.Output(ctx, cmd); err != nil {
			return fmt.Errorf("multica agent create %s: %w", name, err)
		}
	}
	return nil
}

func mergeEnv(a, b map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}
