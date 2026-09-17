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
	"github.com/noelzappy/fleet/internal/platform"
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
	p, err := requireBox("orchestrator init")
	if err != nil {
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

	secret := func(name, gen string) platform.Step {
		return platform.Step{
			Name:  "secret " + name,
			Check: "grep -q '^" + name + "=' " + config.SecretsFile,
			Apply: "echo " + name + "=$(" + gen + ") >> " + config.SecretsFile,
		}
	}
	if _, err := runSteps(ctx, []platform.Step{
		{Name: "multica CLI", Check: "command -v multica >/dev/null",
			Apply: "curl -fsSL https://raw.githubusercontent.com/multica-ai/multica/main/scripts/install.sh | bash"},
		{Name: "multica checkout (latest release tag)", Check: "test -d " + dir + "/.git",
			Apply: "git clone --depth 1 " + multicaRepo + " " + dir + " && cd " + dir + " && git fetch --tags --depth 1 && git checkout -q $(git tag -l 'v*' --sort=-v:refname | head -1)"},
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
	} {
		b, err := templates.Render(f.tmpl, data)
		if err != nil {
			return err
		}
		if err := shell.WriteFile(f.dst, b, f.mode); err != nil {
			return err
		}
	}
	spec := serviceSpec(data.ConfigPath)
	files, err := p.ServiceFiles(spec)
	if err != nil {
		return err
	}
	for _, f := range files {
		if err := shell.WriteFile(f.Path, f.Data, f.Mode); err != nil {
			return err
		}
	}
	jobs := p.Jobs(spec)

	api := "http://" + bind + ":8080"
	app := "http://" + bind + ":3000"
	if _, err := runSteps(ctx, []platform.Step{
		{Name: "multica server up (" + app + ")", Check: "curl -fsS " + api + "/readyz >/dev/null 2>&1",
			Apply: "cd " + dir + " && " + compose + " up -d && for i in $(seq 1 90); do curl -fsS " + api + "/readyz >/dev/null 2>&1 && exit 0; sleep 2; done; echo 'multica did not become ready' >&2; exit 1"},
		// `multica setup self-host` would also start a browser sign-in and time out on a
		// headless box, so the two config keys are set directly.
		{Name: "multica CLI points at this server", Check: "multica config show 2>/dev/null | grep -q " + shell.Quote(api),
			Apply: "multica config set server_url " + api + " && multica config set app_url " + app},
	}); err != nil {
		return err
	}
	if err := ensureLogin(ctx, api, app, dir, compose); err != nil {
		return err
	}
	if err := shell.Run(ctx, p.ReloadCmd(spec), nil); err != nil {
		return err
	}

	if _, err := runSteps(ctx, []platform.Step{
		// Commits agents make are authored with the box's git identity; it must be the owner's.
		{Name: "git identity is the owner's", Check: `test -n "$(git config --global user.name)" && test -n "$(git config --global user.email)"`,
			Apply: `echo "set the owner's git identity on this box first: git config --global user.name '...' && git config --global user.email '...'" >&2; exit 1`},
	}); err != nil {
		return err
	}
	if err := ensureWorkspace(ctx); err != nil {
		return err
	}
	changed, err := ensureOwnerCommits(ctx, api)
	if err != nil {
		return err
	}
	// The daemon reconciles the commit hook in its repo caches at startup; a running
	// daemon keeps the old hook until restarted (in-flight runs re-queue).
	if changed && shell.Check(ctx, p.ActiveCheck(jobs[0])) {
		fmt.Fprintln(os.Stderr, "● restart daemon so the hook change takes effect")
		if err := shell.Run(ctx, p.StopCmd(jobs[0])+" && "+p.StartCmd(jobs[0]), nil); err != nil {
			return err
		}
	}
	if _, err := runSteps(ctx, []platform.Step{
		{Name: "repo registered", Check: "multica repo list --output json | grep -q " + shell.Quote(cfg.Project.Repo),
			Apply: "multica repo add https://github.com/" + shell.Quote(cfg.Project.Repo)},
		{Name: "daemon running", Check: p.ActiveCheck(jobs[0]), Apply: p.StartCmd(jobs[0])},
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
	if _, err := runSteps(ctx, []platform.Step{
		{Name: "sync timer running", Check: p.ActiveCheck(jobs[1]), Apply: p.StartCmd(jobs[1])},
	}); err != nil {
		return err
	}
	cmd.Printf("Multica is up: %s (over Tailscale). Next: fleet github init, fleet issues sync <file>, fleet status\n", app)
	return nil
}

// orchRun is what the systemd unit executes: the Multica daemon in the foreground,
// with the flags derived from fleet.yaml kept here rather than in the unit file.
func orchRun(cmd *cobra.Command, _ []string) error {
	if _, err := requireBox("orchestrator run"); err != nil {
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

// serviceSpec describes the two jobs fleet manages, for whichever service manager
// the platform has. They exec the same fleet binary that ran `orchestrator init`,
// so a build in ./bin works as well as an installed ~/.local/bin/fleet.
func serviceSpec(configPath string) platform.Spec {
	name := cfg.Orchestrator.ServiceName
	bin := shell.Quote(selfPath())
	return platform.Spec{
		Daemon: platform.Job{Name: name, Description: "Multica agent daemon for " + cfg.Project.Name + " (fleet)",
			Exec: bin + " -c " + shell.Quote(configPath) + " orchestrator run", WorkingDir: cfg.Project.Root},
		Sync: platform.Job{Name: name + "-sync", Description: "fleet sync for " + cfg.Project.Name + ": GitHub issues ⇄ Multica (one tick)",
			Exec: bin + " -c " + shell.Quote(configPath) + " sync", WorkingDir: cfg.Project.Root, Interval: cfg.Orchestrator.SyncInterval},
	}
}

// selfPath is this binary's absolute path (symlinks resolved), or ~/.local/bin/fleet.
func selfPath() string {
	exe, err := os.Executable()
	if err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			return real
		}
		return exe
	}
	return config.ExpandPath("~/.local/bin/fleet")
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

// ensureLogin signs the multica CLI in. With orchestrator.owner_email set it needs no
// browser: the email-code exchange is plain HTTP, the code is printed to the backend
// log when no mail server is configured, and a PAT is minted over the API. Without
// owner_email it falls back to pasting a PAT from the web UI.
// authCheck exits 0 iff the CLI holds a token: `auth status` exits 0 either way.
const authCheck = "multica auth status 2>&1 | grep -qiv 'not authenticated'"

func ensureLogin(ctx context.Context, api, app, dir, compose string) error {
	if shell.Check(ctx, authCheck) {
		fmt.Fprintln(os.Stderr, "✓ multica login")
		return nil
	}
	fmt.Fprintln(os.Stderr, "● multica login")
	email := cfg.Orchestrator.OwnerEmail
	if email == "" {
		return shell.Run(ctx, fmt.Sprintf(`echo "Open %s (over Tailscale) and sign up. With no mail server the sign-in code is in the backend log:"; echo "  cd %s && %s logs backend | grep -i code"; echo "Create a token under Settings → API Token and paste it below (set orchestrator.owner_email to skip this)."; multica login --token`, app, dir, compose), nil)
	}
	post := func(path, body, bearer string) (map[string]any, error) {
		cmd := "curl -fsS -X POST -H 'Content-Type: application/json'"
		if bearer != "" {
			cmd += " -H " + shell.Quote("Authorization: Bearer "+bearer)
		}
		out, err := shell.OutputInput(ctx, cmd+" --data-binary @- "+shell.Quote(api+path), body)
		if err != nil {
			return nil, fmt.Errorf("POST %s: %w", path, err)
		}
		var m map[string]any
		return m, decode(out, &m)
	}
	body, _ := json.Marshal(map[string]string{"email": email})
	if _, err := post("/auth/send-code", string(body), ""); err != nil {
		return err
	}
	// The code lands in the backend log within a second or two.
	var code string
	for attempt := 0; attempt < 15 && code == ""; attempt++ {
		out, _ := shell.Output(ctx, fmt.Sprintf("cd %s && %s logs backend --since 2m 2>/dev/null | grep -F %s | tail -1 | grep -oE '[0-9]{6}$'", dir, compose, shell.Quote("Verification code for "+email+":")))
		code = strings.TrimSpace(out)
		if code == "" && !shell.DryRun {
			time.Sleep(2 * time.Second)
		}
		if shell.DryRun {
			code = "<code>"
		}
	}
	if code == "" {
		return fmt.Errorf("no verification code for %s in the backend log — is a mail server configured (RESEND_API_KEY/SMTP_HOST)? Then sign in by hand: multica login --token", email)
	}
	body, _ = json.Marshal(map[string]string{"email": email, "code": code})
	resp, err := post("/auth/verify-code", string(body), "")
	if err != nil {
		return err
	}
	jwt := str(resp["token"])
	if jwt == "" && !shell.DryRun {
		return fmt.Errorf("verify-code returned no token")
	}
	body, _ = json.Marshal(map[string]any{"name": "fleet " + cfg.Project.Name, "expires_in_days": 365})
	resp, err = post("/api/tokens", string(body), jwt)
	if err != nil {
		return err
	}
	pat := str(resp["token"])
	if pat == "" && !shell.DryRun {
		return fmt.Errorf("token creation returned no token")
	}
	// Keep the PAT out of the printed command line: hand it over through a 0600 file.
	tmp := config.ExpandPath("~/.config/fleet/multica-pat.tmp")
	if err := shell.WriteFile(tmp, []byte(pat), 0o600); err != nil {
		return err
	}
	defer os.Remove(tmp)
	return shell.Run(ctx, `multica login --token "$(cat `+shell.Quote(tmp)+`)"`, nil)
}

// ensureOwnerCommits turns off Multica's Co-authored-by hook for the workspace.
// The daemon installs a prepare-commit-msg hook that appends
// "Co-authored-by: multica-agent" to every agent commit, and GitHub keeps that
// trailer through a squash merge. Commits must be the owner's, so the workspace
// setting co_authored_by_enabled is set to false (the daemon re-reads it live).
// The CLI has no flag for it; this goes through the API with the CLI's own token.
func ensureOwnerCommits(ctx context.Context, api string) (changed bool, err error) {
	token, wsID, err := multicaCLIAuth()
	if err != nil {
		return false, err
	}
	// The PAT travels in a 0600 header file so it never appears in the printed command.
	hdr := config.ExpandPath("~/.config/fleet/multica-auth.tmp")
	if err := shell.WriteFile(hdr, []byte("Authorization: Bearer "+token+"\n"), 0o600); err != nil {
		return false, err
	}
	defer os.Remove(hdr)
	url := shell.Quote(api + "/api/workspaces/" + wsID)
	out, err := shell.Output(ctx, "curl -fsS -H @"+shell.Quote(hdr)+" "+url)
	if err != nil {
		return false, fmt.Errorf("GET workspace: %w", err)
	}
	var ws map[string]any
	if err := decode(out, &ws); err != nil {
		return false, fmt.Errorf("GET workspace: %w", err)
	}
	settings, _ := ws["settings"].(map[string]any)
	if settings == nil {
		settings = map[string]any{}
	}
	if v, ok := settings["co_authored_by_enabled"].(bool); ok && !v {
		fmt.Fprintln(os.Stderr, "✓ agent commits carry no Co-authored-by trailer")
		return false, nil
	}
	fmt.Fprintln(os.Stderr, "● agent commits carry no Co-authored-by trailer")
	settings["co_authored_by_enabled"] = false
	body, _ := json.Marshal(map[string]any{"settings": settings})
	if _, err := shell.OutputInput(ctx, "curl -fsS -X PATCH -H 'Content-Type: application/json' -H @"+shell.Quote(hdr)+" --data-binary @- "+url, string(body)); err != nil {
		return false, fmt.Errorf("PATCH workspace settings: %w", err)
	}
	return true, nil
}

// multicaCLIAuth reads the token and default workspace the multica CLI stored at login.
func multicaCLIAuth() (token, workspaceID string, err error) {
	if shell.DryRun {
		return "<token>", "<workspace-id>", nil
	}
	b, err := os.ReadFile(config.ExpandPath("~/.multica/config.json"))
	if err != nil {
		return "", "", fmt.Errorf("multica CLI config: %w", err)
	}
	var c struct {
		Token       string `json:"token"`
		WorkspaceID string `json:"workspace_id"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return "", "", fmt.Errorf("multica CLI config: %w", err)
	}
	if c.Token == "" || c.WorkspaceID == "" {
		return "", "", fmt.Errorf("multica CLI config has no token/workspace — run the login and workspace steps first")
	}
	return c.Token, c.WorkspaceID, nil
}
