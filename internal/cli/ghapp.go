package cli

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/noelzappy/fleet/internal/config"
	"github.com/noelzappy/fleet/internal/ghapp"
	"github.com/noelzappy/fleet/internal/platform"
	"github.com/noelzappy/fleet/internal/shell"
	"github.com/noelzappy/fleet/internal/ui"
	"github.com/spf13/cobra"
)

// fleet github app create|use and fleet github token: run agents on a scoped GitHub App
// instead of the owner's personal login (github.auth: app). See README › GitHub App.

func githubAppCmd() *cobra.Command {
	c := &cobra.Command{Use: "app", Short: "Run the fleet on a scoped GitHub App instead of a personal login"}
	var listen, code string
	create := &cobra.Command{
		Use:   "create",
		Short: "register the App from a manifest (browser handshake) and wait for it to be installed on the repo",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return appCreate(context.Background(), listen, code)
		},
	}
	create.Flags().StringVar(&listen, "listen", "", "address for the handshake page (default: dashboard_bind:8787, else 127.0.0.1:8787)")
	create.Flags().StringVar(&code, "code", "", "finish with the ?code= from the redirect URL if the browser couldn't reach the handshake page")
	use := &cobra.Command{
		Use:   "use",
		Short: "switch to the App: box mode takes over the machine; isolation: project confines it to this repo",
		RunE:  func(cmd *cobra.Command, _ []string) error { return appUse(context.Background()) },
	}
	unuse := &cobra.Command{
		Use:   "unuse",
		Short: "undo `use`: remove the git helper, gh wrapper and PATH blocks (project or box mode)",
		RunE:  func(cmd *cobra.Command, _ []string) error { return appUnuse(context.Background()) },
	}
	var appID int64
	var keyFile string
	var force bool
	imp := &cobra.Command{
		Use:   "import",
		Short: "adopt an existing GitHub App from its App ID and a private key (no new App is registered)",
		Long: `Use this when the App already exists on GitHub but this machine has lost its key or
state (~/.config/fleet/gh-app.*), or when you registered the App by hand. Find the App ID on
the App's settings page (Developer settings > GitHub Apps > your App > About), and generate a
key under "Private keys". fleet checks the key against GitHub, refuses an App with forbidden
permissions, stores both, and looks for the installation on ` + "`project.repo`" + `.`,
		Example: "  fleet github app import --app-id 4995391 --key ~/Downloads/paylte-fleet.2026-09-19.private-key.pem",
		Args:    cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return appImport(context.Background(), appID, keyFile, force)
		},
	}
	imp.Flags().Int64Var(&appID, "app-id", 0, "the App's numeric ID (required)")
	imp.Flags().StringVar(&keyFile, "key", "", "path to the App's private key, a .pem file (required)")
	imp.Flags().BoolVar(&force, "force", false, "replace the App already recorded on this machine")
	_ = imp.MarkFlagRequired("app-id")
	_ = imp.MarkFlagRequired("key")
	c.AddCommand(create, use, unuse, imp)
	return c
}

func githubTokenCmd() *cobra.Command {
	var gitCredential bool
	c := &cobra.Command{
		Use:    "token [get|store|erase]",
		Short:  "print a GitHub App installation token (cached, refreshed 10 min before expiry)",
		Hidden: false,
		Args:   cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			shell.Quiet = true // git and the gh wrapper call this; traces would reach agent output
			if gitCredential {
				// git sends `get` (and later store/erase) plus key=value lines on stdin.
				if len(args) == 0 || args[0] != "get" {
					return nil
				}
				host := ""
				sc := bufio.NewScanner(os.Stdin)
				for sc.Scan() {
					if k, v, ok := strings.Cut(sc.Text(), "="); ok && k == "host" {
						host = v
					}
				}
				if host != "github.com" {
					return nil
				}
			}
			tok, err := appToken(context.Background())
			if err != nil {
				return err
			}
			if gitCredential {
				fmt.Print(ghapp.CredentialOutput(tok))
			} else {
				fmt.Println(tok)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&gitCredential, "git-credential", false, "act as a git credential helper")
	return c
}

var ghAPI = "https://api.github.com" // a var so tests can point it at a local server

var codeRE = regexp.MustCompile(`^[A-Za-z0-9_-]{8,}$`)

func appCreate(ctx context.Context, listen, code string) error {
	st, err := ghapp.LoadState()
	if err == nil && st.InstallationID != 0 {
		ui.Errf("✓ GitHub App %s installed on %s (installation %d)\n", st.Slug, cfg.Project.Repo, st.InstallationID)
		return nil
	}
	if err != nil { // no App yet: register one
		owner, _, _ := strings.Cut(cfg.Project.Repo, "/")
		ownerType := "User"
		if out, err := shell.Output(ctx, "curl -fsS "+shell.Quote(ghAPI+"/users/"+owner)); err == nil {
			var u struct{ Type string }
			if decode(out, &u) == nil && u.Type != "" {
				ownerType = u.Type
			}
		}
		if listen == "" {
			host := cfg.Orchestrator.DashboardBind
			if net.ParseIP(host) == nil {
				host = "127.0.0.1"
			}
			listen = net.JoinHostPort(host, "8787")
		}
		if code == "" {
			if code, err = manifestHandshake(ctx, listen, ownerType, owner); err != nil {
				return err
			}
		}
		if st, err = convertManifest(ctx, code); err != nil {
			return err
		}
	} else {
		ui.Errf("✓ GitHub App %s registered\n", st.Slug)
	}
	return waitInstall(ctx, st)
}

// manifestHandshake serves a page that posts the manifest to GitHub, then waits for
// GitHub to redirect back with a one-hour code. The page must be reachable from the
// browser: it binds to dashboard_bind (127.0.0.1 when the browser is on this machine, the
// Tailscale IP when it isn't; ufw allows tailscale0), else 127.0.0.1.
func manifestHandshake(ctx context.Context, listen, ownerType, owner string) (string, error) {
	stateBytes := make([]byte, 16)
	if _, err := rand.Read(stateBytes); err != nil {
		return "", err
	}
	state := hex.EncodeToString(stateBytes)
	base := "http://" + listen
	manifest, _ := json.Marshal(ghapp.Manifest(cfg.GitHub.AppSlug, cfg.Project.Repo, base+"/callback"))
	if shell.DryRun {
		ui.Errf("→ serve %s/ → POST %s\n  manifest: %s\n", base, ghapp.NewURL(ownerType, owner, "<state>"), manifest)
		return "<code>", nil
	}
	codes := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `<!doctype html><title>fleet: create GitHub App</title>
<form id=f method=post action="%s"><input type=hidden name=manifest value="%s">
<p>Creating GitHub App <b>%s</b> for %s…</p><button>Continue to GitHub</button></form>
<script>document.getElementById('f').submit()</script>`,
			html.EscapeString(ghapp.NewURL(ownerType, owner, state)), html.EscapeString(string(manifest)),
			html.EscapeString(cfg.GitHub.AppSlug), html.EscapeString(cfg.Project.Repo))
	})
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != state || !codeRE.MatchString(q.Get("code")) {
			http.Error(w, "state mismatch or bad code", http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, `<!doctype html><title>fleet</title><p>App registered. Return to the terminal: fleet will print the install link.</p>`)
		select {
		case codes <- q.Get("code"):
		default:
		}
	})
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return "", fmt.Errorf("listen %s: %w (pass --listen, or finish with --code)", listen, err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	defer srv.Close()
	ui.Errf("● open %s/ in your browser and click \"Create GitHub App\"\n", base)
	ui.Errf("  if GitHub's redirect can't load, copy the code= value from its URL and run: fleet github app create --code <code>\n")
	select {
	case c := <-codes:
		return c, nil
	case <-time.After(time.Hour):
		return "", fmt.Errorf("no redirect from GitHub within an hour")
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func convertManifest(ctx context.Context, code string) (ghapp.State, error) {
	var st ghapp.State
	if !codeRE.MatchString(code) && !shell.DryRun {
		return st, fmt.Errorf("code %q doesn't look like a manifest code", code)
	}
	out, err := shell.Output(ctx, "curl -fsS -X POST "+ghHeaders()+" "+shell.Quote(ghAPI+"/app-manifests/"+code+"/conversions"))
	if err != nil {
		return st, fmt.Errorf("manifest conversion (codes expire after an hour): %w", err)
	}
	var resp struct {
		ID       int64  `json:"id"`
		Slug     string `json:"slug"`
		ClientID string `json:"client_id"`
		HTMLURL  string `json:"html_url"`
		PEM      string `json:"pem"`
	}
	if err := decode(out, &resp); err != nil {
		return st, fmt.Errorf("manifest conversion: %w", err)
	}
	if shell.DryRun {
		resp.ID, resp.Slug, resp.ClientID, resp.PEM = 1, cfg.GitHub.AppSlug, "<client-id>", "<pem>"
	}
	if resp.ID == 0 || resp.PEM == "" || resp.ClientID == "" {
		return st, fmt.Errorf("manifest conversion returned no app id, client id or key")
	}
	if err := shell.WriteFile(cfg.GitHub.PrivateKeyPath, []byte(resp.PEM), 0o600); err != nil {
		return st, err
	}
	st = ghapp.State{ID: resp.ID, Slug: resp.Slug, ClientID: resp.ClientID, HTMLURL: resp.HTMLURL}
	if err := shell.WriteFile(ghapp.StatePath, st.JSON(), 0o600); err != nil {
		return st, err
	}
	ui.Errf("✓ GitHub App %s registered (id %d); private key at %s\n", st.Slug, st.ID, cfg.GitHub.PrivateKeyPath)
	return st, nil
}

// waitInstall prints the install link and polls until the App is installed on the repo.
func waitInstall(ctx context.Context, st ghapp.State) error {
	ui.Errf("● install it on %s only: https://github.com/apps/%s/installations/new\n", cfg.Project.Repo, st.Slug)
	for attempt := 0; ; attempt++ {
		shell.Silent = true // a 404 every 5s is the expected answer until the App is installed
		id, err := repoInstallation(ctx, st)
		shell.Silent = false
		ui.LiveDone()
		if err == nil && id != 0 {
			st.InstallationID = id
			if err := shell.WriteFile(ghapp.StatePath, st.JSON(), 0o600); err != nil {
				return err
			}
			ui.Errf("✓ installed on %s (installation %d). Next on the box: fleet github app use\n", cfg.Project.Repo, id)
			return nil
		}
		if shell.DryRun {
			return nil
		}
		if attempt >= 180 {
			return fmt.Errorf("not installed on %s after 15 minutes; re-run `fleet github app create` once it is", cfg.Project.Repo)
		}
		ui.Live("  waiting for the install… %ds (gives up after 15 min)", attempt*5)
		time.Sleep(5 * time.Second)
	}
}

func repoInstallation(ctx context.Context, st ghapp.State) (int64, error) {
	out, err := appAPI(ctx, st, "GET", "/repos/"+cfg.Project.Repo+"/installation", "")
	if err != nil {
		return 0, err
	}
	var resp struct {
		ID int64 `json:"id"`
	}
	return resp.ID, decode(out, &resp)
}

// appAPI calls the GitHub API authenticated as the App (JWT). The JWT travels in a 0600
// header file so it never appears in a trace or a process list.
func appAPI(ctx context.Context, st ghapp.State, method, path, body string) (string, error) {
	var pemBytes []byte
	if !shell.DryRun {
		var err error
		if pemBytes, err = os.ReadFile(cfg.GitHub.PrivateKeyPath); err != nil {
			return "", fmt.Errorf("App private key: %w", err)
		}
	}
	return appAPIWithKey(ctx, st, pemBytes, method, path, body)
}

// appAPIWithKey is appAPI with the key in hand, for `app import`, which verifies a key
// before it replaces the one on disk.
func appAPIWithKey(ctx context.Context, st ghapp.State, pemBytes []byte, method, path, body string) (string, error) {
	var jwt string
	if !shell.DryRun {
		var err error
		if jwt, err = ghapp.JWT(st.ClientID, pemBytes, time.Now()); err != nil {
			return "", err
		}
	}
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	name := filepath.Join(filepath.Dir(ghapp.StatePath), ".jwt-"+hex.EncodeToString(suffix))
	if err := shell.WriteFile(name, []byte("Authorization: Bearer "+jwt+"\n"), 0o600); err != nil {
		return "", err
	}
	if !shell.DryRun {
		defer os.Remove(name)
	}
	cmd := "curl -fsS -X " + method + " " + ghHeaders() + " -H @" + shell.Quote(name)
	if body != "" {
		return shell.OutputInput(ctx, cmd+" -H 'Content-Type: application/json' --data-binary @- "+shell.Quote(ghAPI+path), body)
	}
	return shell.Output(ctx, cmd+" "+shell.Quote(ghAPI+path))
}

func ghHeaders() string {
	return "-H 'Accept: application/vnd.github+json' -H 'X-GitHub-Api-Version: 2022-11-28'"
}

// appToken returns a cached installation token for this repo, minting one when the
// cache has less than 10 minutes left. The token is limited to the one repository, and
// refused if GitHub granted it a forbidden permission.
func appToken(ctx context.Context) (string, error) {
	if shell.DryRun {
		return "<installation-token>", nil
	}
	if t := ghapp.LoadToken(); t.Fresh(time.Now()) {
		return t.Token, nil
	}
	st, err := ghapp.LoadState()
	if err != nil {
		return "", err
	}
	if st.InstallationID == 0 {
		return "", fmt.Errorf("GitHub App %s isn't installed on %s yet: run `fleet github app create`", st.Slug, cfg.Project.Repo)
	}
	_, repoName, _ := strings.Cut(cfg.Project.Repo, "/")
	body, _ := json.Marshal(map[string]any{"repositories": []string{repoName}})
	out, err := appAPI(ctx, st, "POST", "/app/installations/"+strconv.FormatInt(st.InstallationID, 10)+"/access_tokens", string(body))
	if err != nil {
		return "", fmt.Errorf("mint installation token: %w", err)
	}
	var resp struct {
		Token       string            `json:"token"`
		ExpiresAt   time.Time         `json:"expires_at"`
		Permissions map[string]string `json:"permissions"`
	}
	if err := decode(out, &resp); err != nil || resp.Token == "" {
		return "", fmt.Errorf("mint installation token: unexpected response")
	}
	if bad := ghapp.DenyPermissions(resp.Permissions); len(bad) > 0 {
		return "", fmt.Errorf("refusing token: GitHub App %s has %s — remove them in the App's settings", st.Slug, strings.Join(bad, ", "))
	}
	b, _ := json.Marshal(ghapp.Token{Token: resp.Token, ExpiresAt: resp.ExpiresAt})
	if err := shell.WriteFile(ghapp.TokenPath, b, 0o600); err != nil {
		return "", err
	}
	return resp.Token, nil
}

// realGH finds the gh binary behind fleet's wrapper.
func realGH() string {
	for _, p := range []string{"/usr/bin/gh", "/usr/local/bin/gh", "/opt/homebrew/bin/gh"} {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return "gh"
}

// personalLoginCheck exits 0 iff gh holds a login of its own (hosts.yml or keyring),
// ignoring any App token in the environment.
func personalLoginCheck(gh string) string {
	return "env -u GH_TOKEN -u GITHUB_TOKEN " + shell.Quote(gh) + " auth status >/dev/null 2>&1"
}

func appUse(ctx context.Context) error {
	p, err := requireBox("github app use")
	if err != nil {
		return err
	}
	if cfg.GitHub.Auth != "app" {
		return fmt.Errorf("set github.auth: app in fleet.yaml first")
	}
	st, err := ghapp.LoadState()
	if err != nil && !shell.DryRun {
		return err
	}
	if st.InstallationID == 0 && !shell.DryRun {
		return fmt.Errorf("GitHub App isn't installed on %s yet: run `fleet github app create`", cfg.Project.Repo)
	}
	gh := realGH()
	if st.RealGH != gh && !shell.DryRun {
		st.RealGH = gh
		if err := shell.WriteFile(ghapp.StatePath, st.JSON(), 0o600); err != nil {
			return err
		}
	}
	abs, _ := filepath.Abs(cfgPath)
	self := selfPath()

	wrapper := ghapp.Wrapper(self, abs, gh)
	wrapperPath := filepath.Join(ghapp.WrapperDir, "gh")
	if cur, err := os.ReadFile(wrapperPath); err == nil && string(cur) == string(wrapper) {
		ui.Errln("✓ gh wrapper")
	} else {
		ui.Errln("● gh wrapper")
		if err := shell.WriteFile(wrapperPath, wrapper, 0o755); err != nil {
			return err
		}
	}

	helper := credentialHelper(self, abs)
	var steps []platform.Step
	if projectScoped() {
		steps = projectUseSteps(cfg.Project.Repo, helper, self, abs)
	} else {
		steps = boxUseSteps(p.ProfileFiles(), helper, self, abs, gh)
	}
	if _, err := runSteps(ctx, steps); err != nil {
		return err
	}
	if projectScoped() {
		ui.Errf("GitHub App %s is in use for %s only. Your gh login and git setup are untouched; fleet's own commands and its agents get the App through the gh wrapper.\nAgents run as you, so this scopes accidents, not a determined agent (README › GitHub App). Restart the daemon: fleet pause --hard && fleet up\n", st.Slug, cfg.Project.Repo)
	} else {
		ui.Errf("GitHub App %s is in use. Restart the daemon so agents start with the new PATH: fleet pause --hard && fleet up\n", st.Slug)
	}
	return nil
}

// projectScoped reports whether the App is confined to this project (github.isolation:
// project) instead of taking over the whole box.
func projectScoped() bool {
	return cfg.GitHub.Auth == "app" && cfg.GitHub.Isolation == "project"
}

const profileMarker = "# fleet: gh via GitHub App"

func credentialHelper(self, cfgAbs string) string {
	return "!" + self + " -c " + cfgAbs + " github token --git-credential"
}

// repoCredentialKeys are the git config subsections that match a repo's HTTPS URL, with
// and without .git: git matches URL paths by whole segments, so `…/repo` doesn't cover
// `…/repo.git`.
func repoCredentialKeys(repo string) []string {
	return []string{"credential.https://github.com/" + repo, "credential.https://github.com/" + repo + ".git"}
}

// projectUseSteps confines the App to one repo. The helper is attached to that repo's URL
// only (useHttpPath makes git match on the path; the empty `helper =` drops the keychain
// helper for that URL), so every other repo keeps using your own credentials. Nothing is
// removed: not your gh login, not ~/.git-credentials, not your shell profile.
func projectUseSteps(repo, helper, self, cfgAbs string) []platform.Step {
	var steps []platform.Step
	for _, key := range repoCredentialKeys(repo) {
		steps = append(steps, platform.Step{
			Name: "git pushes to " + repo + " use App tokens (" + strings.TrimPrefix(key, "credential.https://github.com/") + ")",
			Check: "test \"$(git config --global --get-all " + shell.Quote(key+".helper") + " | tail -1)\" = " + shell.Quote(helper) +
				" && test \"$(git config --global --get " + shell.Quote(key+".useHttpPath") + ")\" = true",
			Apply: "git config --global --unset-all " + shell.Quote(key+".helper") + "; git config --global --add " + shell.Quote(key+".helper") + " '' && git config --global --add " + shell.Quote(key+".helper") + " " + shell.Quote(helper) +
				" && git config --global " + shell.Quote(key+".useHttpPath") + " true",
		})
	}
	wrapper := filepath.Join(ghapp.WrapperDir, "gh")
	return append(steps,
		platform.Step{
			Name:  "App token mints and is scoped",
			Check: shell.Quote(self) + " -c " + shell.Quote(cfgAbs) + " github token >/dev/null",
			Apply: shell.Quote(self) + " -c " + shell.Quote(cfgAbs) + " github token >/dev/null",
		},
		platform.Step{
			Name:  "the gh wrapper reaches the repo as the App",
			Check: "test \"$(" + shell.Quote(wrapper) + " api repos/" + repo + " -q .full_name 2>/dev/null)\" = " + shell.Quote(repo),
			Apply: shell.Quote(wrapper) + " api repos/" + repo + " -q .full_name",
		},
	)
}

// boxUseSteps takes over the whole machine, for a dedicated fleet box.
func boxUseSteps(profiles []string, helper, self, cfgAbs, gh string) []platform.Step {
	var steps []platform.Step
	for _, prof := range profiles {
		block := fmt.Sprintf("\n%s\nexport PATH=\"%s:$PATH\"\n", profileMarker, strings.Replace(ghapp.WrapperDir, config.ExpandPath("~"), "$HOME", 1))
		steps = append(steps, platform.Step{
			Name:  "gh wrapper first on PATH (" + prof + ")",
			Check: "grep -qxF " + shell.Quote(profileMarker) + " " + prof,
			Apply: "printf " + shell.Quote(block) + " >> " + prof,
		})
	}
	return append(steps,
		platform.Step{
			Name:  "git pushes with App tokens",
			Check: "git config --global --get-all credential.https://github.com.helper | tail -1 | grep -qxF " + shell.Quote(helper),
			Apply: "git config --global --unset-all credential.https://github.com.helper; git config --global --add credential.https://github.com.helper '' && git config --global --add credential.https://github.com.helper " + shell.Quote(helper),
		},
		platform.Step{
			Name:  "no stored git credentials",
			Check: "! test -s ~/.git-credentials",
			Apply: "rm -f ~/.git-credentials",
		},
		platform.Step{
			// Agents run as this user: any personal login here would let them bypass the App.
			Name:  "personal gh login removed from this box",
			Check: "! " + personalLoginCheck(gh),
			Apply: "env -u GH_TOKEN -u GITHUB_TOKEN " + shell.Quote(gh) + " auth logout --hostname github.com",
		},
		platform.Step{
			Name:  "App token mints and is scoped",
			Check: shell.Quote(self) + " -c " + shell.Quote(cfgAbs) + " github token >/dev/null",
			Apply: shell.Quote(self) + " -c " + shell.Quote(cfgAbs) + " github token >/dev/null",
		},
		platform.Step{
			Name:  "gh (through the wrapper) reaches the repo as the App",
			Check: "test \"$(gh api repos/" + cfg.Project.Repo + " -q .full_name 2>/dev/null)\" = " + shell.Quote(cfg.Project.Repo),
			Apply: "gh api repos/" + cfg.Project.Repo + " -q .full_name",
		},
	)
}

// unuseSteps removes what either mode of `github app use` added: the repo-scoped git
// stanzas, the box-wide helper (only when it is fleet's), the PATH blocks in the profile
// files and the gh wrapper. It can't restore a personal gh login that box mode removed.
func unuseSteps(repo string, profiles []string) []platform.Step {
	var steps []platform.Step
	for _, key := range repoCredentialKeys(repo) {
		steps = append(steps, platform.Step{
			Name:  "git config for " + strings.TrimPrefix(key, "credential.https://github.com/") + " removed",
			Check: "! git config --global --get-all " + shell.Quote(key+".helper") + " >/dev/null 2>&1",
			Apply: "git config --global --remove-section " + shell.Quote(key),
		})
	}
	steps = append(steps, platform.Step{
		Name:  "box-wide git credential helper removed",
		Check: "! git config --global --get-all credential.https://github.com.helper 2>/dev/null | grep -q 'github token --git-credential'",
		Apply: "git config --global --unset-all credential.https://github.com.helper",
	})
	for _, prof := range profiles {
		steps = append(steps, platform.Step{
			Name:  "gh wrapper PATH block removed (" + prof + ")",
			Check: "! grep -qxF " + shell.Quote(profileMarker) + " " + prof + " 2>/dev/null",
			Apply: "perl -0pi -e " + shell.Quote(`s/\n`+profileMarker+`\nexport PATH=[^\n]*\n//g`) + " " + prof,
		})
	}
	wrapper := filepath.Join(ghapp.WrapperDir, "gh")
	return append(steps, platform.Step{
		Name:  "gh wrapper removed",
		Check: "! test -e " + shell.Quote(wrapper),
		Apply: "rm -f " + shell.Quote(wrapper),
	})
}

func appUnuse(ctx context.Context) error {
	p, err := requireBox("github app unuse")
	if err != nil {
		return err
	}
	if _, err := runSteps(ctx, unuseSteps(cfg.Project.Repo, p.ProfileFiles())); err != nil {
		return err
	}
	ui.Errln("GitHub App setup removed. If box mode removed your personal gh login, sign in again: gh auth login")
	return nil
}

// requireAppIsolation is the guard sync and orchestrator init run on an auth: app box:
// the App must be installed and no personal login may exist.
func requireAppIsolation(ctx context.Context) error {
	if cfg.GitHub.Auth != "app" || shell.DryRun {
		return nil
	}
	st, err := ghapp.LoadState()
	if err != nil {
		return err
	}
	if st.InstallationID == 0 {
		return fmt.Errorf("github.auth is app but the App isn't installed: run `fleet github app create`")
	}
	if projectScoped() {
		return nil // a personal login is expected here; the App is confined to this repo instead
	}
	gh := st.RealGH
	if gh == "" {
		gh = realGH()
	}
	if shell.Check(ctx, personalLoginCheck(gh)) {
		return fmt.Errorf("github.auth is app but a personal gh login exists on this box; agents could use it to bypass the App. Run `fleet github app use`")
	}
	return nil
}

// appImport adopts an App that already exists: it proves the key by calling GET /app as the
// App, applies the same permission limits a minted token gets, then stores the key and state
// and finds (or waits for) the installation on the project repo. The key on disk is replaced
// only after GitHub has accepted the new one.
func appImport(ctx context.Context, appID int64, keyFile string, force bool) error {
	if appID <= 0 {
		return fmt.Errorf("--app-id must be the App's numeric ID")
	}
	old, oldErr := ghapp.LoadState()
	if oldErr == nil && old.ID != 0 && old.ID != appID && !force {
		return fmt.Errorf("this machine already has App %s (id %d); pass --force to replace it", old.Slug, old.ID)
	}
	pemBytes, err := os.ReadFile(config.ExpandPath(keyFile))
	if err != nil && !shell.DryRun {
		return fmt.Errorf("read key: %w", err)
	}
	// GitHub accepts the numeric App ID as the JWT issuer, which is all we know before /app
	// tells us the client ID.
	probe := ghapp.State{ClientID: strconv.FormatInt(appID, 10)}
	if !shell.DryRun {
		if _, err := ghapp.JWT(probe.ClientID, pemBytes, time.Now()); err != nil {
			return fmt.Errorf("%s isn't a usable private key: %w", keyFile, err)
		}
	}
	out, err := appAPIWithKey(ctx, probe, pemBytes, "GET", "/app", "")
	if err != nil {
		return fmt.Errorf("GitHub rejected App %d with this key (wrong App ID, or a key that was revoked or belongs to another App): %w", appID, err)
	}
	var app struct {
		ID          int64             `json:"id"`
		Slug        string            `json:"slug"`
		ClientID    string            `json:"client_id"`
		HTMLURL     string            `json:"html_url"`
		Permissions map[string]string `json:"permissions"`
	}
	if err := decode(out, &app); err != nil {
		return fmt.Errorf("GET /app: %w", err)
	}
	if shell.DryRun {
		app.ID, app.Slug, app.ClientID = appID, cfg.GitHub.AppSlug, "<client-id>"
	}
	if app.ID != appID || app.Slug == "" || app.ClientID == "" {
		return fmt.Errorf("GET /app returned an unexpected App (id %d, slug %q)", app.ID, app.Slug)
	}
	if bad := ghapp.DenyPermissions(app.Permissions); len(bad) > 0 {
		return fmt.Errorf("refusing to import App %s: it has %s. Remove them in the App's settings (agents must not be able to change workflows, secrets or admin settings)", app.Slug, strings.Join(bad, ", "))
	}
	if err := shell.WriteFile(cfg.GitHub.PrivateKeyPath, pemBytes, 0o600); err != nil {
		return err
	}
	st := ghapp.State{ID: app.ID, Slug: app.Slug, ClientID: app.ClientID, HTMLURL: app.HTMLURL}
	if oldErr == nil && old.ID == app.ID { // same App: keep what a previous `use` recorded
		st.InstallationID, st.RealGH = old.InstallationID, old.RealGH
	}
	if err := shell.WriteFile(ghapp.StatePath, st.JSON(), 0o600); err != nil {
		return err
	}
	ui.Errf("✓ GitHub App %s (id %d) imported; private key at %s\n", st.Slug, st.ID, cfg.GitHub.PrivateKeyPath)
	if id, err := repoInstallation(ctx, st); err == nil && id != 0 {
		st.InstallationID = id
		if err := shell.WriteFile(ghapp.StatePath, st.JSON(), 0o600); err != nil {
			return err
		}
		ui.Errf("✓ installed on %s (installation %d). Next: fleet github app use\n", cfg.Project.Repo, id)
		return nil
	}
	st.InstallationID = 0 // a stale id from an earlier App or install would mislead `use`
	if err := shell.WriteFile(ghapp.StatePath, st.JSON(), 0o600); err != nil {
		return err
	}
	return waitInstall(ctx, st)
}
