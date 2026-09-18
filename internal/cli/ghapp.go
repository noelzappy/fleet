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
		Short: "switch this box to the App: gh wrapper, git credential helper, remove the personal gh login, verify",
		RunE:  func(cmd *cobra.Command, _ []string) error { return appUse(context.Background()) },
	}
	c.AddCommand(create, use)
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

const ghAPI = "https://api.github.com"

var codeRE = regexp.MustCompile(`^[A-Za-z0-9_-]{8,}$`)

func appCreate(ctx context.Context, listen, code string) error {
	st, err := ghapp.LoadState()
	if err == nil && st.InstallationID != 0 {
		fmt.Fprintf(os.Stderr, "✓ GitHub App %s installed on %s (installation %d)\n", st.Slug, cfg.Project.Repo, st.InstallationID)
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
		fmt.Fprintf(os.Stderr, "✓ GitHub App %s registered\n", st.Slug)
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
		fmt.Fprintf(os.Stderr, "→ serve %s/ → POST %s\n  manifest: %s\n", base, ghapp.NewURL(ownerType, owner, "<state>"), manifest)
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
	fmt.Fprintf(os.Stderr, "● open %s/ in your browser and click \"Create GitHub App\"\n", base)
	fmt.Fprintf(os.Stderr, "  if GitHub's redirect can't load, copy the code= value from its URL and run: fleet github app create --code <code>\n")
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
	fmt.Fprintf(os.Stderr, "✓ GitHub App %s registered (id %d); private key at %s\n", st.Slug, st.ID, cfg.GitHub.PrivateKeyPath)
	return st, nil
}

// waitInstall prints the install link and polls until the App is installed on the repo.
func waitInstall(ctx context.Context, st ghapp.State) error {
	fmt.Fprintf(os.Stderr, "● install it on %s only: https://github.com/apps/%s/installations/new\n", cfg.Project.Repo, st.Slug)
	for attempt := 0; ; attempt++ {
		id, err := repoInstallation(ctx, st)
		if err == nil && id != 0 {
			st.InstallationID = id
			if err := shell.WriteFile(ghapp.StatePath, st.JSON(), 0o600); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "✓ installed on %s (installation %d). Next on the box: fleet github app use\n", cfg.Project.Repo, id)
			return nil
		}
		if shell.DryRun {
			return nil
		}
		if attempt >= 180 {
			return fmt.Errorf("not installed on %s after 15 minutes; re-run `fleet github app create` once it is", cfg.Project.Repo)
		}
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
	var jwt string
	if !shell.DryRun {
		pemBytes, err := os.ReadFile(cfg.GitHub.PrivateKeyPath)
		if err != nil {
			return "", fmt.Errorf("App private key: %w", err)
		}
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
		fmt.Fprintln(os.Stderr, "✓ gh wrapper")
	} else {
		fmt.Fprintln(os.Stderr, "● gh wrapper")
		if err := shell.WriteFile(wrapperPath, wrapper, 0o755); err != nil {
			return err
		}
	}

	const marker = "# fleet: gh via GitHub App"
	helper := "!" + self + " -c " + abs + " github token --git-credential"
	var steps []platform.Step
	for _, prof := range p.ProfileFiles() {
		block := fmt.Sprintf("\\n%s\\nexport PATH=\"%s:$PATH\"\\n", marker, strings.Replace(ghapp.WrapperDir, config.ExpandPath("~"), "$HOME", 1))
		steps = append(steps, platform.Step{
			Name:  "gh wrapper first on PATH (" + prof + ")",
			Check: "grep -qxF " + shell.Quote(marker) + " " + prof,
			Apply: "printf " + shell.Quote(block) + " >> " + prof,
		})
	}
	steps = append(steps,
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
			Check: shell.Quote(self) + " -c " + shell.Quote(abs) + " github token >/dev/null",
			Apply: shell.Quote(self) + " -c " + shell.Quote(abs) + " github token >/dev/null",
		},
		platform.Step{
			Name:  "gh (through the wrapper) reaches the repo as the App",
			Check: "test \"$(gh api repos/" + cfg.Project.Repo + " -q .full_name 2>/dev/null)\" = " + shell.Quote(cfg.Project.Repo),
			Apply: "gh api repos/" + cfg.Project.Repo + " -q .full_name",
		},
	)
	if _, err := runSteps(ctx, steps); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "GitHub App %s is in use. Restart the daemon so agents start with the new PATH: fleet pause --hard && fleet up\n", st.Slug)
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
	gh := st.RealGH
	if gh == "" {
		gh = realGH()
	}
	if shell.Check(ctx, personalLoginCheck(gh)) {
		return fmt.Errorf("github.auth is app but a personal gh login exists on this box; agents could use it to bypass the App. Run `fleet github app use`")
	}
	return nil
}
