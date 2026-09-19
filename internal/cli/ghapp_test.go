package cli

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noelzappy/fleet/internal/config"
	"github.com/noelzappy/fleet/internal/ghapp"
	"github.com/noelzappy/fleet/internal/platform"
	"github.com/noelzappy/fleet/internal/shell"
)

// git runs against throwaway system/global config so the test never touches the real one.
func gitEnv(t *testing.T) (env []string, app string) {
	t.Helper()
	dir := t.TempDir()
	personal := filepath.Join(dir, "personal.sh")
	app = filepath.Join(dir, "app.sh")
	os.WriteFile(personal, []byte("#!/bin/sh\ncat >/dev/null\necho username=me\necho password=PERSONAL\n"), 0o755)
	os.WriteFile(app, []byte("#!/bin/sh\ncat >/dev/null\necho username=x-access-token\necho password=APPTOKEN\n"), 0o755)
	system := filepath.Join(dir, "system.gitconfig")
	os.WriteFile(system, []byte("[credential]\n\thelper = !"+personal+"\n"), 0o644) // stands in for osxkeychain
	global := filepath.Join(dir, "global.gitconfig")
	return append(os.Environ(), "GIT_CONFIG_SYSTEM="+system, "GIT_CONFIG_GLOBAL="+global, "HOME="+dir), app
}

func sh(t *testing.T, env []string, cmd string) string {
	t.Helper()
	c := exec.Command("bash", "-c", cmd)
	c.Env = env
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", cmd, err, out)
	}
	return string(out)
}

func credFor(t *testing.T, env []string, path string) string {
	return sh(t, env, "printf 'protocol=https\\nhost=github.com\\npath="+path+"\\n\\n' | git credential fill")
}

func passes(env []string, cmd string) bool {
	c := exec.Command("bash", "-c", cmd)
	c.Env = env
	return c.Run() == nil
}

func TestProjectUseScopesGitCredentials(t *testing.T) {
	env, app := gitEnv(t)
	steps := projectUseSteps("o/r", "!"+app, "true", "fleet.yaml")
	var gitSteps []platform.Step
	for _, s := range steps {
		if strings.HasPrefix(s.Name, "git pushes") {
			gitSteps = append(gitSteps, s)
		}
	}
	if len(gitSteps) != 2 {
		t.Fatalf("want a step for the repo URL with and without .git, got %d", len(gitSteps))
	}
	for _, s := range gitSteps {
		if passes(env, s.Check) {
			t.Errorf("%s: check passes before apply", s.Name)
		}
		sh(t, env, s.Apply)
		if !passes(env, s.Check) {
			t.Errorf("%s: check fails after apply", s.Name)
		}
	}
	for _, path := range []string{"o/r.git", "o/r"} {
		if got := credFor(t, env, path); !strings.Contains(got, "password=APPTOKEN") {
			t.Errorf("%s should use the App, got:\n%s", path, got)
		}
	}
	for _, path := range []string{"o/other.git", "someone/else.git", "o/r2.git"} {
		if got := credFor(t, env, path); !strings.Contains(got, "password=PERSONAL") {
			t.Errorf("%s must keep your own credentials, got:\n%s", path, got)
		}
	}
	// Nothing box-wide: no helper on the bare github.com URL.
	if out := sh(t, env, "git config --global --get-all credential.https://github.com.helper || true"); out != "" {
		t.Errorf("project mode wrote a box-wide helper: %s", out)
	}

	// unuse puts everything back.
	for _, s := range unuseSteps("o/r", nil)[:2] {
		if passes(env, s.Check) {
			t.Errorf("%s: unuse check passes before apply", s.Name)
		}
		sh(t, env, s.Apply)
		if !passes(env, s.Check) {
			t.Errorf("%s: unuse check fails after apply", s.Name)
		}
	}
	if got := credFor(t, env, "o/r.git"); !strings.Contains(got, "password=PERSONAL") {
		t.Errorf("after unuse o/r should be back on your credentials, got:\n%s", got)
	}
}

func TestProjectUseKeepsPersonalSetup(t *testing.T) {
	for _, s := range projectUseSteps("o/r", "!h", "fleet", "fleet.yaml") {
		for _, banned := range []string{"auth logout", ".git-credentials", "credential.https://github.com.helper", ".zprofile", ".bash_profile"} {
			if strings.Contains(s.Apply, banned) {
				t.Errorf("project mode step %q touches %q: %s", s.Name, banned, s.Apply)
			}
		}
	}
}

func testKeyPEM(t *testing.T) []byte {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
}

// importEnv points fleet at a fake GitHub API and throwaway state, and returns the paths
// import writes to.
func importEnv(t *testing.T, appJSON string, installed bool) (keyOut, keyIn string) {
	t.Helper()
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, "no jwt", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/app":
			fmt.Fprint(w, appJSON)
		case "/repos/o/r/installation":
			if !installed {
				http.NotFound(w, r)
				return
			}
			fmt.Fprint(w, `{"id": 777}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	oldAPI, oldState, oldCfg := ghAPI, ghapp.StatePath, cfg
	t.Cleanup(func() { ghAPI, ghapp.StatePath, cfg = oldAPI, oldState, oldCfg })
	ghAPI = srv.URL
	ghapp.StatePath = filepath.Join(dir, "gh-app.json")
	keyOut = filepath.Join(dir, "gh-app.pem")
	cfg = &config.Fleet{Project: config.Project{Repo: "o/r"}, GitHub: config.GitHub{PrivateKeyPath: keyOut}}
	keyIn = filepath.Join(dir, "downloaded.pem")
	if err := os.WriteFile(keyIn, testKeyPEM(t), 0o600); err != nil {
		t.Fatal(err)
	}
	return keyOut, keyIn
}

const goodApp = `{"id": 42, "slug": "widgets-fleet", "client_id": "Iv1.abc", "html_url": "https://github.com/apps/widgets-fleet",
  "permissions": {"contents": "write", "pull_requests": "write", "issues": "write", "metadata": "read"}}`

func TestAppImport(t *testing.T) {
	ctx := context.Background()
	keyOut, keyIn := importEnv(t, goodApp, true)
	if err := appImport(ctx, 42, keyIn, false); err != nil {
		t.Fatal(err)
	}
	st, err := ghapp.LoadState()
	if err != nil || st.ID != 42 || st.Slug != "widgets-fleet" || st.ClientID != "Iv1.abc" || st.InstallationID != 777 {
		t.Fatalf("state = %+v, %v", st, err)
	}
	fi, err := os.Stat(keyOut)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("key not stored 0600: %v %v", fi, err)
	}
	// Importing the same App again is fine and keeps what `use` recorded.
	st.RealGH = "/opt/homebrew/bin/gh"
	if err := shell.WriteFile(ghapp.StatePath, st.JSON(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := appImport(ctx, 42, keyIn, false); err != nil {
		t.Fatal(err)
	}
	if again, _ := ghapp.LoadState(); again.RealGH != "/opt/homebrew/bin/gh" {
		t.Errorf("re-import dropped real_gh: %+v", again)
	}
	// A different App needs --force.
	if err := appImport(ctx, 43, keyIn, false); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Errorf("replacing another App without --force: %v", err)
	}
}

func TestAppImportRefusals(t *testing.T) {
	ctx := context.Background()
	t.Run("not a key", func(t *testing.T) {
		keyOut, keyIn := importEnv(t, goodApp, true)
		os.WriteFile(keyIn, []byte("hello"), 0o600)
		if err := appImport(ctx, 42, keyIn, false); err == nil || !strings.Contains(err.Error(), "usable private key") {
			t.Errorf("err = %v", err)
		}
		if _, err := os.Stat(keyOut); err == nil {
			t.Error("a bad key was stored")
		}
	})
	t.Run("wrong app id", func(t *testing.T) {
		keyOut, keyIn := importEnv(t, goodApp, true) // GitHub says the key belongs to App 42
		if err := appImport(ctx, 99, keyIn, false); err == nil || !strings.Contains(err.Error(), "unexpected App") {
			t.Errorf("err = %v", err)
		}
		if _, err := os.Stat(keyOut); err == nil {
			t.Error("key stored for the wrong App")
		}
	})
	t.Run("forbidden permissions", func(t *testing.T) {
		keyOut, keyIn := importEnv(t, `{"id":42,"slug":"s","client_id":"c","permissions":{"contents":"write","workflows":"write"}}`, true)
		if err := appImport(ctx, 42, keyIn, false); err == nil || !strings.Contains(err.Error(), "workflows") {
			t.Errorf("err = %v", err)
		}
		if _, err := os.Stat(keyOut); err == nil {
			t.Error("key stored for an over-permissioned App")
		}
	})
	t.Run("existing key survives a rejected import", func(t *testing.T) {
		keyOut, keyIn := importEnv(t, `{"message":"bad"}`, true)
		os.WriteFile(keyOut, []byte("existing"), 0o600)
		os.WriteFile(keyIn, testKeyPEM(t), 0o600)
		if err := appImport(ctx, 42, keyIn, false); err == nil {
			t.Error("a rejected import succeeded")
		}
		if err := appImport(ctx, 0, keyIn, false); err == nil {
			t.Error("app id 0 accepted")
		}
		if b, _ := os.ReadFile(keyOut); string(b) != "existing" {
			t.Errorf("existing key was overwritten: %q", b)
		}
	})
}
