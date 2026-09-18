package cli

import (
	"strings"
	"testing"
)

func TestCheckBind(t *testing.T) {
	tests := []struct {
		bind, wantErr string
		dryRun        bool
	}{
		{"127.0.0.1", "", false},
		{"", "specific IPv4", false},
		{"0.0.0.0", "never 0.0.0.0", false},
		{"::1", "specific IPv4", false},
		{"localhost", "specific IPv4", false},
		{"100.64.0.1:3000", "specific IPv4", false},
		{"192.0.2.1", "isn't an address on this machine", false}, // TEST-NET-1: never assigned
		{"192.0.2.1", "", true},                                  // dry-run: the box may be elsewhere
		{"0.0.0.0", "never 0.0.0.0", true},
	}
	for _, tt := range tests {
		err := checkBind(tt.bind, tt.dryRun)
		switch {
		case tt.wantErr == "" && err != nil:
			t.Errorf("checkBind(%q, dry=%v) = %v, want nil", tt.bind, tt.dryRun, err)
		case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
			t.Errorf("checkBind(%q, dry=%v) = %v, want error containing %q", tt.bind, tt.dryRun, err, tt.wantErr)
		}
	}
}

func TestStablePath(t *testing.T) {
	yes := func(string) bool { return true }
	no := func(string) bool { return false }
	tests := []struct {
		name, exe string
		same      func(string) bool
		want      string
	}{
		{"homebrew arm64", "/opt/homebrew/Cellar/fleet/0.1.0/bin/fleet", yes, "/opt/homebrew/bin/fleet"},
		{"linuxbrew", "/home/linuxbrew/.linuxbrew/Cellar/fleet/0.1.1/bin/fleet", yes, "/home/linuxbrew/.linuxbrew/bin/fleet"},
		{"link points elsewhere", "/opt/homebrew/Cellar/fleet/0.1.0/bin/fleet", no, "/opt/homebrew/Cellar/fleet/0.1.0/bin/fleet"},
		{"make install", "/Users/e/.local/bin/fleet", yes, "/Users/e/.local/bin/fleet"},
		{"dev build", "/Users/e/workspace/fleet/bin/fleet", yes, "/Users/e/workspace/fleet/bin/fleet"},
	}
	for _, tt := range tests {
		if got := stablePath(tt.exe, tt.same); got != tt.want {
			t.Errorf("%s: stablePath(%q) = %q, want %q", tt.name, tt.exe, got, tt.want)
		}
	}
}
