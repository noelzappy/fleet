package platform

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/noelzappy/fleet/internal/config"
)

var full = config.Machine{NodeVersion: "24", Pnpm: "11", Docker: true, Tailscale: true, Firewall: true, AlwaysOn: true, TurboRemote: &config.Turbo{Team: "t"}}
var minimal = config.Machine{NodeVersion: "24", Pnpm: "11"}

func names(t *testing.T, steps []Step) map[string]int {
	t.Helper()
	m := map[string]int{}
	for i, s := range steps {
		if s.Name == "" || s.Check == "" || s.Apply == "" {
			t.Errorf("incomplete step %+v", s)
		}
		if _, dup := m[s.Name]; dup {
			t.Errorf("duplicate step name %q", s.Name)
		}
		m[s.Name] = i
	}
	return m
}

func TestBootstrapSteps(t *testing.T) {
	tests := []struct {
		name    string
		p       Platform
		m       config.Machine
		present []string
		absent  []string
	}{
		{"linux full", Linux{}, full,
			[]string{"apt packages", "docker", "fnm", "node 24", "pnpm 11", "gh", "tailscale up", "firewall", "SSH key installed for this user", "SSH password login disabled", "TURBO_TEAM in secrets file"}, nil},
		{"linux minimal", Linux{}, minimal,
			[]string{"apt packages", "fnm", "node 24", "fleet directories and secrets file"},
			[]string{"docker", "tailscale", "firewall", "SSH password login disabled", "TURBO_TEAM in secrets file"}},
		{"darwin full", Darwin{}, full,
			[]string{"homebrew", "brew packages", "docker (OrbStack)", "node 24", "pnpm 11", "never sleep", "tailscaled", "tailscale up", "application firewall on, stealth mode", "remote login (sshd) on", "SSH key installed for this user", "SSH password login disabled", "TURBO_TEAM in secrets file"}, nil},
		{"darwin minimal", Darwin{}, minimal,
			[]string{"homebrew", "brew packages", "node 24"},
			[]string{"docker (OrbStack)", "tailscaled", "SSH password login disabled", "never sleep"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := names(t, tt.p.BootstrapSteps(tt.m, "/home/u/fleet/x"))
			for _, n := range tt.present {
				if _, ok := got[n]; !ok {
					t.Errorf("missing step %q", n)
				}
			}
			for _, n := range tt.absent {
				if _, ok := got[n]; ok {
					t.Errorf("unexpected step %q", n)
				}
			}
		})
	}
	for _, p := range []Platform{Linux{}, Darwin{}} {
		idx := names(t, p.BootstrapSteps(full, "/r"))
		if idx["SSH key installed for this user"] > idx["SSH password login disabled"] {
			t.Errorf("%s: password login disabled before key check", p.Name())
		}
		if idx["homebrew"] > idx["brew packages"] && p.Name() == "darwin" {
			t.Error("darwin: packages before homebrew")
		}
	}
	for _, s := range (Linux{}).BootstrapSteps(config.Machine{Firewall: true}, "/r") {
		if s.Name == "firewall" && strings.Contains(s.Apply+s.Check, "tailscale0") {
			t.Error("tailscale0 rule present with tailscale off")
		}
	}
}

func spec() Spec {
	return Spec{
		Daemon: Job{Name: "fleet-multica", Description: "daemon", Exec: "~/.local/bin/fleet -c /p/fleet.yaml orchestrator run", WorkingDir: "/p"},
		Sync:   Job{Name: "fleet-multica-sync", Description: "sync", Exec: "~/.local/bin/fleet -c /p/fleet.yaml sync", WorkingDir: "/p", Interval: "2m"},
	}
}

func TestServiceFiles(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	lin, err := Linux{}.ServiceFiles(spec())
	if err != nil {
		t.Fatal(err)
	}
	if len(lin) != 3 {
		t.Fatalf("linux: %d files", len(lin))
	}
	want := map[string]string{
		"/home/u/.config/systemd/user/fleet-multica.service":      "ExecStart=%h/.local/bin/fleet -c /p/fleet.yaml orchestrator run\nRestart=on-failure",
		"/home/u/.config/systemd/user/fleet-multica-sync.service": "Type=oneshot",
		"/home/u/.config/systemd/user/fleet-multica-sync.timer":   "OnUnitActiveSec=2m\nUnit=fleet-multica-sync.service",
	}
	for _, f := range lin {
		if !strings.Contains(string(f.Data), want[f.Path]) {
			t.Errorf("%s:\n%s\nmissing %q", f.Path, f.Data, want[f.Path])
		}
		if strings.Contains(string(f.Data), "~/") {
			t.Errorf("%s: unexpanded ~", f.Path)
		}
	}

	mac, err := Darwin{}.ServiceFiles(spec())
	if err != nil {
		t.Fatal(err)
	}
	if len(mac) != 2 {
		t.Fatalf("darwin: %d files", len(mac))
	}
	daemon, sync := string(mac[0].Data), string(mac[1].Data)
	for _, s := range []string{"<string>fleet-multica</string>", "<key>KeepAlive</key><true/>", `exec $HOME/.local/bin/fleet -c /p/fleet.yaml orchestrator run`, "/home/u/Library/Logs/fleet/fleet-multica.log"} {
		if !strings.Contains(daemon, s) {
			t.Errorf("daemon plist missing %q:\n%s", s, daemon)
		}
	}
	if !strings.Contains(sync, "<key>StartInterval</key><integer>120</integer>") || strings.Contains(sync, "KeepAlive") {
		t.Errorf("sync plist:\n%s", sync)
	}
	if mac[0].Path != "/home/u/Library/LaunchAgents/fleet-multica.plist" {
		t.Errorf("plist path %s", mac[0].Path)
	}
	if _, err := exec.LookPath("plutil"); err == nil {
		for _, f := range mac {
			cmd := exec.Command("plutil", "-lint", "-")
			cmd.Stdin = strings.NewReader(string(f.Data))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("plutil -lint %s: %s", f.Path, out)
			}
		}
	}
	if _, err := (Darwin{}).ServiceFiles(Spec{Sync: Job{Name: "s", Interval: "often"}}); err == nil {
		t.Error("bad interval accepted")
	}
}

func TestServiceCommands(t *testing.T) {
	s := spec()
	tests := []struct {
		p    Platform
		jobs []string
		want map[string]string // substring expected in each command
	}{
		{Linux{}, []string{"fleet-multica", "fleet-multica-sync.timer"}, map[string]string{
			"start": "systemctl --user enable --now fleet-multica fleet-multica-sync.timer",
			"stop":  "systemctl --user stop fleet-multica fleet-multica-sync.timer",
			"is":    "systemctl --user is-active fleet-multica || true",
			"check": "systemctl --user is-active --quiet fleet-multica",
			"load":  "daemon-reload",
		}},
		{Darwin{}, []string{"fleet-multica", "fleet-multica-sync"}, map[string]string{
			"start": "launchctl kickstart gui/$(id -u)/fleet-multica-sync",
			"stop":  "launchctl bootout gui/$(id -u)/fleet-multica",
			"is":    "state = running",
			"check": "launchctl print gui/$(id -u)/fleet-multica >/dev/null",
			"load":  "launchctl bootstrap gui/$(id -u)",
		}},
	}
	for _, tt := range tests {
		if got := tt.p.Jobs(s); strings.Join(got, ",") != strings.Join(tt.jobs, ",") {
			t.Errorf("%s jobs = %v", tt.p.Name(), got)
		}
		got := map[string]string{
			"start": tt.p.StartCmd(tt.jobs...), "stop": tt.p.StopCmd(tt.jobs...),
			"is": tt.p.IsActiveCmd(tt.jobs[0]), "check": tt.p.ActiveCheck(tt.jobs[0]), "load": tt.p.ReloadCmd(s),
		}
		for k, want := range tt.want {
			if !strings.Contains(got[k], want) {
				t.Errorf("%s %s = %q, want substring %q", tt.p.Name(), k, got[k], want)
			}
		}
	}
	if _, err := For("windows"); err == nil {
		t.Error("windows accepted")
	}
}
