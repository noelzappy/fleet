package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noelzappy/fleet/internal/platform"
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
