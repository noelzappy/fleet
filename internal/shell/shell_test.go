package shell

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

func TestQuote(t *testing.T) {
	cases := []string{
		"plain",
		"",
		"with space",
		"Run `pnpm gate` in this directory.",
		"$HOME ${X} $(id)",
		"it's",
		"multi\nline\n\n## Depends on\n- #3\n",
		`back\slash "dq"`,
		"[ADMIN] wallet freeze/unfreeze",
		"; rm -rf /tmp/nope",
	}
	for _, in := range cases {
		out, err := exec.Command("bash", "-c", "printf %s "+Quote(in)).Output()
		if err != nil {
			t.Fatalf("Quote(%q) = %s: bash: %v", in, Quote(in), err)
		}
		if string(out) != in {
			t.Errorf("Quote(%q) = %s round-tripped to %q", in, Quote(in), out)
		}
	}
	if got := Quote("agent-ready"); got != "agent-ready" {
		t.Errorf("safe word quoted: %s", got)
	}
}

func TestRedact(t *testing.T) {
	cases := map[string]string{
		"curl -H 'Authorization: Bearer eyJhbGciOi.eyJlbWFp.sig-_x' http://x":                   "curl -H 'Authorization: Bearer ***' http://x",
		"multica login --token mul_c77d4477abcdef":                                              "multica login --token mul_***",
		"git push https://x:ghs_abcdefgh12345@github.com":                                       "git push https://x:ghs_***@github.com",
		"curl https://api.telegram.org/bot123456789:AAH-secret_token-value-0123456789abc/getMe": "curl https://api.telegram.org/bot***/getMe",
		"→ curl -fsS http://127.0.0.1:8080/readyz":                                              "→ curl -fsS http://127.0.0.1:8080/readyz",
	}
	for in, want := range cases {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q)\n got  %q\n want %q", in, got, want)
		}
	}
}

func TestPathPrefixComesAfterTheLoginShell(t *testing.T) {
	old := PathPrefix
	defer func() { PathPrefix = old }()
	PathPrefix = "/opt/fleet-test/bin"
	// bash -l reads the user's profile, which may prepend its own dirs; the prefix must
	// still win, and must not leak into a command without it.
	out, err := Output(context.Background(), "echo $PATH")
	if err != nil || !strings.HasPrefix(out, "/opt/fleet-test/bin:") {
		t.Fatalf("PATH = %q, err %v", out, err)
	}
	PathPrefix = ""
	if out, _ := Output(context.Background(), "echo $PATH"); strings.HasPrefix(out, "/opt/fleet-test/bin") {
		t.Errorf("prefix leaked with PathPrefix unset: %q", out)
	}
}
