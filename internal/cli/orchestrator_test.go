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
