package cli

import (
	"reflect"
	"strings"
	"testing"

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
