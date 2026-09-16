package cli

import (
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/noelzappy/fleet/internal/config"
)

func TestCountLabels(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    map[string]int
		wantErr bool
	}{
		{"empty output", "", map[string]int{}, false},
		{"no issues", "[]", map[string]int{}, false},
		{"counts across issues", `[{"labels":[{"name":"agent-ready"},{"name":"wave:ui"}]},{"labels":[{"name":"agent-ready"}]},{"labels":[]}]`,
			map[string]int{"agent-ready": 2, "wave:ui": 1}, false},
		{"garbage", "not json", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := countLabels(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDependsBody(t *testing.T) {
	got := dependsBody("do the thing", []string{"A", "B"}, map[string]string{"A": "3", "B": "7"})
	want := "do the thing\n\n## Depends on\n- #3\n- #7\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBootstrapSteps(t *testing.T) {
	names := func(steps []step) map[string]bool {
		m := map[string]bool{}
		for _, s := range steps {
			if s.name == "" || s.check == "" || s.apply == "" {
				t.Errorf("incomplete step %+v", s)
			}
			if m[s.name] {
				t.Errorf("duplicate step name %q", s.name)
			}
			m[s.name] = true
		}
		return m
	}
	full := config.Machine{NodeVersion: "24", Pnpm: "11", Docker: true, Tailscale: true, Firewall: true, TurboRemote: &config.Turbo{Team: "t"}}
	tests := []struct {
		name    string
		m       config.Machine
		present []string
		absent  []string
	}{
		{"everything on", full,
			[]string{"docker", "tailscale up", "firewall", "SSH key installed for this user", "SSH password login disabled", "TURBO_TEAM in secrets file"}, nil},
		{"minimal", config.Machine{NodeVersion: "24", Pnpm: "11"},
			[]string{"apt packages", "fnm", "node 24", "pnpm 11", "gh", "fleet directories and secrets file"},
			[]string{"docker", "tailscale", "firewall", "SSH password login disabled", "TURBO_TEAM in secrets file"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := names(bootstrapSteps(tt.m, "/home/u/fleet/x"))
			for _, n := range tt.present {
				if !got[n] {
					t.Errorf("missing step %q", n)
				}
			}
			for _, n := range tt.absent {
				if got[n] {
					t.Errorf("unexpected step %q", n)
				}
			}
		})
	}
	t.Run("key guard precedes password lockdown", func(t *testing.T) {
		idx := map[string]int{}
		for i, s := range bootstrapSteps(full, "/r") {
			idx[s.name] = i
		}
		if idx["SSH key installed for this user"] > idx["SSH password login disabled"] {
			t.Error("password login disabled before key check")
		}
	})
	t.Run("firewall without tailscale has no tailscale0 rule", func(t *testing.T) {
		for _, s := range bootstrapSteps(config.Machine{Firewall: true}, "/r") {
			if s.name == "firewall" && strings.Contains(s.apply+s.check, "tailscale0") {
				t.Error("tailscale0 rule present with tailscale off")
			}
		}
	})
}

// Every kind's gate invocation must survive hostile prompts and gates as literal words.
func TestHarnessKindsGateRun(t *testing.T) {
	for _, kind := range config.HarnessKinds {
		k, ok := harnessKinds[kind]
		if !ok {
			t.Fatalf("config.HarnessKinds lists %q but cli has no entry", kind)
		}
		cmd := k.gateRun("Run `pnpm gate`; it's $HOME", "pnpm gate", 25*time.Minute)
		if !strings.HasPrefix(cmd, k.bin+" ") && !strings.Contains(cmd, " "+k.bin+" ") {
			t.Errorf("%s: invocation doesn't call %s: %s", kind, k.bin, cmd)
		}
		out, err := exec.Command("bash", "-c", "set -- "+cmd+`; printf '%s\n' "$@"`).Output()
		if err != nil {
			t.Fatalf("%s: bash could not parse %s: %v", kind, cmd, err)
		}
		if !strings.Contains(string(out), "Run `pnpm gate`; it's $HOME\n") {
			t.Errorf("%s: prompt not passed as one literal word:\n%s", kind, out)
		}
	}
	if len(harnessKinds) != len(config.HarnessKinds) {
		t.Errorf("cli knows %d kinds, config allows %d", len(harnessKinds), len(config.HarnessKinds))
	}
}

func TestJSONRows(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantN    int
		wantMore bool
		wantErr  bool
	}{
		{"empty (dry-run)", "", 0, false, false},
		{"bare array", `[{"id":"a"},{"id":"b"}]`, 2, false, false},
		{"wrapped with has_more", `{"issues":[{"id":"a"}],"has_more":true}`, 1, true, false},
		{"wrapped second key", `{"tasks":[{"id":"a"}]}`, 1, false, false},
		{"wrong key", `{"things":[]}`, 0, false, true},
		{"garbage", `nope`, 0, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows, more, err := jsonRows(tt.in, "issues", "tasks")
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if len(rows) != tt.wantN || more != tt.wantMore {
				t.Errorf("got %d rows, more=%v; want %d, %v", len(rows), more, tt.wantN, tt.wantMore)
			}
		})
	}
}

func TestSlugAndPrefix(t *testing.T) {
	for in, want := range map[string]string{
		"[ADMIN] Wallet freeze/unfreeze": "admin-wallet-freeze-unfreeze",
		"  ":                             "task",
		"A very long title that keeps going and going past forty characters": "a-very-long-title-that-keeps-going-and-g",
	} {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"widgets": "WIDG", "my-app": "MYAP", "x": "X", "---": "FLT"} {
		if got := issuePrefix(in); got != want {
			t.Errorf("issuePrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNumStr(t *testing.T) {
	if num(float64(42)) != 42 || num("7") != 7 || num(nil) != 0 || str(float64(3)) != "3" || str("x") != "x" || str(nil) != "" {
		t.Error("num/str conversions")
	}
}
