package shell

import (
	"os/exec"
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
