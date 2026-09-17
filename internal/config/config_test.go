package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const example = "../../fleet.example.yaml"

// fakeHome points ~ at a temp dir so path expansion is deterministic.
func fakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func TestLoadExample(t *testing.T) {
	home := fakeHome(t)
	f, err := Load(example)
	if err != nil {
		t.Fatalf("Load(%s): %v", example, err)
	}
	tests := []struct {
		name string
		got  any
		want any
	}{
		{"project.name", f.Project.Name, "widgets"},
		{"project.repo", f.Project.Repo, "your-org/widgets"},
		{"project.root expands ~", f.Project.Root, filepath.Join(home, "fleet/widgets")},
		{"machine.node", f.Machine.NodeVersion, "24"},
		{"turbo team", f.Machine.TurboRemote.Team, "your-team"},
		{"harness count", len(f.Harnesses), 3},
		{"gemini profiles use antigravity", f.Harnesses[f.Profiles["impl-gemini"].Harness].Kind, "antigravity"},
		{"glm runs through opencode", f.Profiles["impl-glm"].Harness, "opencode"},
		{"profile count", len(f.Profiles), 7},
		{"impl-gemini waves", f.Profiles["impl-gemini"].Waves, []string{"contracts", "ui", "admin", "mobile"}},
		{"wave order", f.Waves[0].Name, "contracts"},
		{"private key expands ~", f.GitHub.PrivateKeyPath, filepath.Join(home, ".config/fleet/gh-app.pem")},
		{"required checks", f.GitHub.RequiredChecks, []string{"gate", "pr-contract"}},
		{"telegram token secret", f.Notify.Telegram.TokenSecret, "TG_TOKEN"},
		{"service name", f.Orchestrator.ServiceName, "fleet-multica"},
		{"orchestrator dir expands ~", f.Orchestrator.Dir, filepath.Join(home, "fleet/multica")},
		{"sync interval", f.Orchestrator.SyncInterval, "2m"},
		{"wave label default", f.Waves[0].WaveLabel(), "wave:contracts"},
		{"wave for label", f.WaveForLabel("wave:ui"), "ui"},
		{"implementers for contracts", f.ProfilesWhere(func(_ string, p Profile) bool { return p.Role == RoleImplementer && contains(p.Waves, "contracts") }), []string{"impl-gemini", "impl-glm"}},
		{"labels default ready", f.Labels.Ready, "agent-ready"},
		{"labels default paused", f.Labels.Paused, "fleet-paused"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !reflect.DeepEqual(tt.got, tt.want) {
				t.Errorf("got %v, want %v", tt.got, tt.want)
			}
		})
	}
}

// fleet.example.yaml exists twice: at the repo root for readers and embedded for `fleet init`.
func TestExampleCopiesMatch(t *testing.T) {
	root, err := os.ReadFile(example)
	if err != nil {
		t.Fatal(err)
	}
	embedded, err := os.ReadFile("../templates/files/fleet.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if string(root) != string(embedded) {
		t.Fatal("fleet.example.yaml differs from internal/templates/files/fleet.example.yaml — copy one over the other")
	}
}

func TestApplyDefaults(t *testing.T) {
	home := fakeHome(t)
	tests := []struct {
		name  string
		in    Fleet
		check func(*Fleet) (got, want any)
	}{
		{"branch", Fleet{}, func(f *Fleet) (any, any) { return f.Project.Branch, "main" }},
		{"root from name", Fleet{Project: Project{Name: "x"}}, func(f *Fleet) (any, any) { return f.Project.Root, filepath.Join(home, "fleet/x") }},
		{"explicit root kept", Fleet{Project: Project{Root: "/srv/x"}}, func(f *Fleet) (any, any) { return f.Project.Root, "/srv/x" }},
		{"gate command", Fleet{}, func(f *Fleet) (any, any) { return f.Gate.Command, "pnpm gate" }},
		{"gate timeout", Fleet{}, func(f *Fleet) (any, any) { return f.Gate.Timeout, "15m" }},
		{"max gate attempts", Fleet{}, func(f *Fleet) (any, any) { return f.Routing.MaxGateAttempts, 3 }},
		{"orchestrator kind", Fleet{}, func(f *Fleet) (any, any) { return f.Orchestrator.Kind, "multica" }},
		{"service name follows kind", Fleet{}, func(f *Fleet) (any, any) { return f.Orchestrator.ServiceName, "fleet-multica" }},
		{"workspace follows project", Fleet{Project: Project{Name: "x"}}, func(f *Fleet) (any, any) { return f.Orchestrator.Workspace, "x" }},
		{"custom label kept", Fleet{Labels: Labels{Stuck: "halp"}}, func(f *Fleet) (any, any) { return f.Labels.Stuck, "halp" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := tt.in
			f.ApplyDefaults()
			f.ApplyDefaults() // idempotent
			if got, want := tt.check(&f); !reflect.DeepEqual(got, want) {
				t.Errorf("got %v, want %v", got, want)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	base := func() Fleet {
		return Fleet{
			Project:   Project{Name: "p", Repo: "o/p"},
			Harnesses: map[string]Harness{"cc": {Kind: "claude-code"}},
			Profiles: map[string]Profile{
				"impl": {Harness: "cc", Role: "implementer", Vendor: "anthropic"},
				"rev":  {Harness: "cc", Role: "reviewer", Vendor: "google"},
			},
			Routing:      Routing{CrossVendorReview: true},
			Gate:         Gate{Timeout: "15m"},
			Orchestrator: Orchestrator{Kind: "multica", SyncInterval: "2m"},
			Waves:        []Wave{{Name: "ui"}},
		}
	}
	tests := []struct {
		name    string
		mutate  func(*Fleet)
		wantErr string
	}{
		{"valid", func(*Fleet) {}, ""},
		{"missing repo", func(f *Fleet) { f.Project.Repo = "" }, "project.repo"},
		{"gemini-cli points at antigravity", func(f *Fleet) { f.Harnesses["cc"] = Harness{Kind: "gemini-cli"} }, "replaced by antigravity"},
		{"unknown kind", func(f *Fleet) { f.Harnesses["cc"] = Harness{Kind: "codex"} }, "not one of"},
		{"antigravity kind ok", func(f *Fleet) { f.Harnesses["cc"] = Harness{Kind: "antigravity"} }, ""},
		{"bad gate timeout", func(f *Fleet) { f.Gate.Timeout = "soon" }, "gate.timeout"},
		{"ao no longer supported", func(f *Fleet) { f.Orchestrator.Kind = "ao" }, "only multica"},
		{"bad sync interval", func(f *Fleet) { f.Orchestrator.SyncInterval = "often" }, "sync_interval"},
		{"bad role", func(f *Fleet) { p := f.Profiles["impl"]; p.Role = "coder"; f.Profiles["impl"] = p }, "must be implementer"},
		{"unknown wave", func(f *Fleet) { p := f.Profiles["impl"]; p.Waves = []string{"nope"}; f.Profiles["impl"] = p }, "not in waves"},
		{"known wave", func(f *Fleet) { p := f.Profiles["impl"]; p.Waves = []string{"ui"}; f.Profiles["impl"] = p }, ""},
		{"bad min_version", func(f *Fleet) { f.Harnesses["cc"] = Harness{Kind: "claude-code", MinVersion: "latest"} }, "min_version"},
		{"good min_version", func(f *Fleet) { f.Harnesses["cc"] = Harness{Kind: "claude-code", MinVersion: "2.1.163"} }, ""},
		{"unknown harness", func(f *Fleet) { p := f.Profiles["impl"]; p.Harness = "nope"; f.Profiles["impl"] = p }, "unknown harness"},
		{"missing vendor", func(f *Fleet) { p := f.Profiles["rev"]; p.Vendor = ""; f.Profiles["rev"] = p }, "vendor is required"},
		{"same-vendor review", func(f *Fleet) { p := f.Profiles["rev"]; p.Vendor = "anthropic"; f.Profiles["rev"] = p }, "cross_vendor_review"},
		{"same-vendor ok when rule off", func(f *Fleet) {
			p := f.Profiles["rev"]
			p.Vendor = "anthropic"
			f.Profiles["rev"] = p
			f.Routing.CrossVendorReview = false
		}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := base()
			tt.mutate(&f)
			err := f.validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("got %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestExpandEnv(t *testing.T) {
	vars := map[string]string{"KEY": "s3cr$t", "HOST": "api.z.ai"}
	lookup := func(n string) (string, bool) { v, ok := vars[n]; return v, ok }
	tests := []struct {
		name    string
		in      map[string]string
		want    map[string]string
		wantErr string
	}{
		{"braced", map[string]string{"T": "${KEY}"}, map[string]string{"T": "s3cr$t"}, ""},
		{"bare", map[string]string{"T": "$KEY"}, map[string]string{"T": "s3cr$t"}, ""},
		{"embedded", map[string]string{"U": "https://${HOST}/anthropic"}, map[string]string{"U": "https://api.z.ai/anthropic"}, ""},
		{"literal", map[string]string{"M": "glm-5.3"}, map[string]string{"M": "glm-5.3"}, ""},
		{"nil map", nil, map[string]string{}, ""},
		{"all missing reported sorted", map[string]string{"A": "${ZED}", "B": "${ALPHA}"}, nil, "ALPHA, ZED"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ExpandEnv(tt.in, lookup)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("got %v, want error containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseEnvFile(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    map[string]string
		wantErr bool
	}{
		{"plain", "A=1\nB=two\n", map[string]string{"A": "1", "B": "two"}, false},
		{"comments and blanks", "# c\n\nA=1\n", map[string]string{"A": "1"}, false},
		{"export prefix", "export A=1", map[string]string{"A": "1"}, false},
		{"quotes stripped", `A="x y"` + "\nB='$z'", map[string]string{"A": "x y", "B": "$z"}, false},
		{"equals in value", "A=b=c", map[string]string{"A": "b=c"}, false},
		{"bad line", "NOPE", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "env")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := ParseEnvFile(path)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
	t.Run("missing file is empty", func(t *testing.T) {
		got, err := ParseEnvFile(filepath.Join(t.TempDir(), "absent"))
		if err != nil || len(got) != 0 {
			t.Fatalf("got %v, %v", got, err)
		}
	})
}

func TestExpandPath(t *testing.T) {
	home := fakeHome(t)
	for in, want := range map[string]string{
		"~":       home,
		"~/a/b":   filepath.Join(home, "a/b"),
		"/abs":    "/abs",
		"rel":     "rel",
		"~user/x": "~user/x",
		"":        "",
	} {
		if got := ExpandPath(in); got != want {
			t.Errorf("ExpandPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveVar(t *testing.T) {
	t.Setenv("BOTH", "from-env")
	t.Setenv("ENV_ONLY", "from-env")
	t.Setenv("BLANK_IN_FILE", "from-env")
	t.Setenv("EMPTY_ENV", "")
	file := map[string]string{"BOTH": "from-file", "FILE_ONLY": "from-file", "BLANK_IN_FILE": "", "EMPTY_ENV": ""}
	tests := []struct {
		name   string
		want   string
		wantOK bool
	}{
		{"BOTH", "from-file", true}, // file wins
		{"FILE_ONLY", "from-file", true},
		{"ENV_ONLY", "from-env", true},
		{"BLANK_IN_FILE", "from-env", true}, // blank placeholder falls through
		{"EMPTY_ENV", "", false},            // empty everywhere is missing
		{"NOWHERE", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := resolveVar(tt.name, file)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("got (%q, %v), want (%q, %v)", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestVersions(t *testing.T) {
	tests := []struct {
		have, min string
		want      bool
	}{
		{"2.1.273 (Claude Code)", "2.1.163", true},
		{"2.1.163", "2.1.163", true},
		{"2.1.162", "2.1.163", false},
		{"2.1.99", "2.1.163", false}, // numeric, not lexical
		{"v1.18.18", "1.1.10", true},
		{"1.1.21", "1.1.28", false},
		{"1.2", "1.2.0", true},
		{"1.10.0", "1.9.9", true},
		{"agy version 1.2.4\nbuild abc", "1.1.28", true},
	}
	for _, tt := range tests {
		have, err := ParseVersion(tt.have)
		if err != nil {
			t.Fatalf("ParseVersion(%q): %v", tt.have, err)
		}
		min, _ := ParseVersion(tt.min)
		if got := VersionAtLeast(have, min); got != tt.want {
			t.Errorf("VersionAtLeast(%q, %q) = %v, want %v", tt.have, tt.min, got, tt.want)
		}
	}
	for _, bad := range []string{"", "latest", "v2"} {
		if _, err := ParseVersion(bad); err == nil {
			t.Errorf("ParseVersion(%q) should fail", bad)
		}
	}
}

func TestLoadHarnessEnv(t *testing.T) {
	home := fakeHome(t)
	path := filepath.Join(t.TempDir(), "fleet.yaml")
	yml := `version: 1
project: { name: p, repo: o/p }
harnesses:
  proxy:
    kind: claude-code
    env_file: ~/.config/fleet/profiles/proxy.env
    env: { ANTHROPIC_AUTH_TOKEN: "${PROXY_KEY}" }
`
	if err := os.WriteFile(path, []byte(yml), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := f.Harnesses["proxy"].EnvFile, filepath.Join(home, ".config/fleet/profiles/proxy.env"); got != want {
		t.Errorf("env_file = %s, want %s", got, want)
	}
	if got := f.Harnesses["proxy"].Env["ANTHROPIC_AUTH_TOKEN"]; got != "${PROXY_KEY}" {
		t.Errorf("env expanded at load time: %s", got)
	}
}
