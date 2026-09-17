// Package config loads fleet.yaml — the per-project contract the CLI operates on.
// Everything project-specific lives here; the binary is project-agnostic.
package config

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Fleet struct {
	Version      int                `yaml:"version"`
	Project      Project            `yaml:"project"`
	Machine      Machine            `yaml:"machine"`
	Harnesses    map[string]Harness `yaml:"harnesses"` // CLI tools agents run through
	Profiles     map[string]Profile `yaml:"profiles"`  // harness + model + role; names appear in PR bodies
	Waves        []Wave             `yaml:"waves"`     // ordered; dispatch priority follows this order
	Routing      Routing            `yaml:"routing"`
	Orchestrator Orchestrator       `yaml:"orchestrator"`
	GitHub       GitHub             `yaml:"github"`
	Notify       Notify             `yaml:"notify"`
	Gate         Gate               `yaml:"gate"`
	Labels       Labels             `yaml:"labels"`
}

type Project struct {
	Name   string `yaml:"name"`
	Repo   string `yaml:"repo"`   // owner/name
	Branch string `yaml:"branch"` // default main
	Root   string `yaml:"root"`   // ~/fleet/<name>
	Docs   string `yaml:"docs"`   // governance docs dir
}

type Machine struct {
	NodeVersion string `yaml:"node"`
	Pnpm        string `yaml:"pnpm"`
	Docker      bool   `yaml:"docker"`
	Tailscale   bool   `yaml:"tailscale"`
	Firewall    bool   `yaml:"firewall"`
	AlwaysOn    bool   `yaml:"always_on"` // dedicated box: bootstrap disables sleep (macOS)
	TurboRemote *Turbo `yaml:"turbo_remote,omitempty"`
}

type Turbo struct {
	TokenEnv string `yaml:"token_env"`
	Team     string `yaml:"team"`
}

type Harness struct {
	Kind    string `yaml:"kind"`    // one of HarnessKinds
	Install string `yaml:"install"` // shell
	Login   string `yaml:"login"`   // interactive; CLI sequences + verifies
	Smoke   string `yaml:"smoke"`   // headless; must exit 0
	// MinVersion is the lowest version fleet will run. harness add/verify/update fail below it.
	// Set it from the tool's security advisories; see README › Security model.
	MinVersion string `yaml:"min_version"`
	Update     string `yaml:"update"` // overrides the kind's default update command
	// AuthCheck exits 0 iff the CLI is signed in; overrides the kind's default (useful
	// when a harness authenticates through env vars or a custom provider instead).
	AuthCheck string            `yaml:"auth_check"`
	Env       map[string]string `yaml:"env"`      // e.g. ANTHROPIC_BASE_URL for GLM
	EnvFile   string            `yaml:"env_file"` // ~/.config/fleet/profiles/<name>.env
}

type Profile struct {
	Harness     string            `yaml:"harness"`
	Model       string            `yaml:"model"`
	Role        string            `yaml:"role"` // implementer | reviewer | fixer
	Vendor      string            `yaml:"vendor"`
	Concurrency int               `yaml:"concurrency"`
	Waves       []string          `yaml:"waves"`
	Env         map[string]string `yaml:"env"`
}

type Wave struct {
	Name  string `yaml:"name"`
	Label string `yaml:"label"`
}

type Routing struct {
	CrossVendorReview bool `yaml:"cross_vendor_review"`
	MaxGateAttempts   int  `yaml:"max_gate_attempts"`
	FixerOnlyLint     bool `yaml:"fixer_only_lint"`
}

// Orchestrator is the execution backend. Only multica is implemented; see
// docs/orchestrator-decision.md for why and for what fleet bolts on around it.
type Orchestrator struct {
	Kind          string `yaml:"kind"`           // multica
	Dir           string `yaml:"dir"`            // clone of multica-ai/multica (compose files); default ~/fleet/multica
	DashboardBind string `yaml:"dashboard_bind"` // IP the web UI and API bind to; your tailscale IP, never 0.0.0.0
	ServiceName   string `yaml:"service_name"`   // systemd unit prefix; default fleet-multica
	Workspace     string `yaml:"workspace"`      // Multica workspace name; default project.name
	SyncInterval  string `yaml:"sync_interval"`  // how often `fleet sync` reconciles GitHub and Multica; default 2m
	OwnerEmail    string `yaml:"owner_email"`    // optional: the only email allowed to sign up to the Multica UI
}

type GitHub struct {
	// Auth is how agents and fleet reach GitHub: "gh" uses the box's gh login (a personal
	// account, fine for a scratch repo); "app" uses short-lived tokens from a GitHub App
	// with no administration or workflows permission. Default gh.
	Auth           string   `yaml:"auth"`
	AppSlug        string   `yaml:"app_slug"`   // name for the App that `fleet github app create` registers
	AppIDEnv       string   `yaml:"app_id_env"` // deprecated: unused, the App's ids live in ~/.config/fleet/gh-app.json
	PrivateKeyPath string   `yaml:"private_key_path"`
	RequiredChecks []string `yaml:"required_checks"`
}

type Notify struct {
	Telegram *Telegram `yaml:"telegram,omitempty"`
	Digest   Digest    `yaml:"digest"`
}

type Telegram struct {
	TokenSecret string `yaml:"token_secret"`
	ChatSecret  string `yaml:"chat_secret"`
}

type Digest struct {
	Cron string `yaml:"cron"`
	TZ   string `yaml:"tz"`
}

type Gate struct {
	Command  string `yaml:"command"`
	Timeout  string `yaml:"timeout"`
	Workflow string `yaml:"workflow"` // GitHub Actions workflow that runs the gate on PRs; default "gate"
}

type Labels struct {
	Ready         string `yaml:"ready"`
	Assist        string `yaml:"assist"`
	HumanRequired string `yaml:"human_required"`
	NeedsHuman    string `yaml:"needs_human"`
	NeedsResource string `yaml:"needs_resource"`
	NeedsContract string `yaml:"needs_contract"`
	BlockedBy     string `yaml:"blocked_by"`
	Stuck         string `yaml:"stuck"`
	Paused        string `yaml:"paused"`
}

func Load(path string) (*Fleet, error) {
	if path == "" {
		path = "fleet.yaml"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var f Fleet
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	f.ApplyDefaults()
	if err := f.validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

// ApplyDefaults fills every unset knob and expands ~ in paths Go writes to directly.
// Safe to call more than once.
func (f *Fleet) ApplyDefaults() {
	if f.Project.Branch == "" {
		f.Project.Branch = "main"
	}
	if f.Project.Root == "" {
		f.Project.Root = filepath.Join("~", "fleet", f.Project.Name)
	}
	f.Project.Root = ExpandPath(f.Project.Root)
	for name, h := range f.Harnesses {
		h.EnvFile = ExpandPath(h.EnvFile)
		f.Harnesses[name] = h
	}
	if f.Routing.MaxGateAttempts == 0 {
		f.Routing.MaxGateAttempts = 3
	}
	if f.Gate.Command == "" {
		f.Gate.Command = "pnpm gate"
	}
	if f.Gate.Timeout == "" {
		f.Gate.Timeout = "15m"
	}
	if f.GitHub.Auth == "" {
		f.GitHub.Auth = "gh"
	}
	if f.GitHub.AppSlug == "" {
		f.GitHub.AppSlug = f.Project.Name + "-fleet"
	}
	if f.GitHub.PrivateKeyPath == "" {
		f.GitHub.PrivateKeyPath = "~/.config/fleet/gh-app.pem"
	}
	f.GitHub.PrivateKeyPath = ExpandPath(f.GitHub.PrivateKeyPath)
	if f.Gate.Workflow == "" {
		f.Gate.Workflow = "gate"
	}
	def := func(p *string, v string) {
		if *p == "" {
			*p = v
		}
	}
	l := &f.Labels
	def(&l.Ready, "agent-ready")
	def(&l.Assist, "agent-assist")
	def(&l.HumanRequired, "human-required")
	def(&l.NeedsHuman, "needs-human")
	def(&l.NeedsResource, "needs-resource")
	def(&l.NeedsContract, "needs-contract")
	def(&l.BlockedBy, "blocked-by")
	def(&l.Stuck, "agent-stuck")
	def(&l.Paused, "fleet-paused")
	if f.Orchestrator.Kind == "" {
		f.Orchestrator.Kind = "multica"
	}
	if f.Orchestrator.ServiceName == "" {
		f.Orchestrator.ServiceName = "fleet-" + f.Orchestrator.Kind
	}
	if f.Orchestrator.Dir == "" {
		f.Orchestrator.Dir = "~/fleet/multica"
	}
	f.Orchestrator.Dir = ExpandPath(f.Orchestrator.Dir)
	if f.Orchestrator.Workspace == "" {
		f.Orchestrator.Workspace = f.Project.Name
	}
	if f.Orchestrator.SyncInterval == "" {
		f.Orchestrator.SyncInterval = "2m"
	}
}

// HarnessKinds are the agent CLIs fleet knows how to install, run headless and check.
var HarnessKinds = []string{"claude-code", "opencode", "antigravity", "codex"}

func (f *Fleet) validate() error {
	if f.Project.Name == "" || f.Project.Repo == "" {
		return fmt.Errorf("project.name and project.repo are required")
	}
	for name, h := range f.Harnesses {
		if h.Kind == "gemini-cli" {
			return fmt.Errorf("harness %q: kind gemini-cli was replaced by antigravity (Google folded Gemini CLI into Antigravity CLI)", name)
		}
		if !contains(HarnessKinds, h.Kind) {
			return fmt.Errorf("harness %q: kind %q is not one of %s", name, h.Kind, strings.Join(HarnessKinds, ", "))
		}
		if h.MinVersion != "" {
			if _, err := ParseVersion(h.MinVersion); err != nil {
				return fmt.Errorf("harness %q: min_version: %w", name, err)
			}
		}
	}
	if _, err := time.ParseDuration(f.Gate.Timeout); err != nil {
		return fmt.Errorf("gate.timeout: %w", err)
	}
	if f.GitHub.Auth != "gh" && f.GitHub.Auth != "app" {
		return fmt.Errorf("github.auth %q: must be gh or app", f.GitHub.Auth)
	}
	if f.Orchestrator.Kind != "multica" {
		return fmt.Errorf("orchestrator.kind %q: only multica is supported (docs/orchestrator-decision.md)", f.Orchestrator.Kind)
	}
	if _, err := time.ParseDuration(f.Orchestrator.SyncInterval); err != nil {
		return fmt.Errorf("orchestrator.sync_interval: %w", err)
	}
	waves := map[string]bool{}
	for _, w := range f.Waves {
		waves[w.Name] = true
	}
	for name, p := range f.Profiles {
		switch p.Role {
		case RoleImplementer, RoleReviewer, RoleFixer:
		default:
			return fmt.Errorf("profile %q: role %q must be implementer, reviewer or fixer", name, p.Role)
		}
		for _, w := range p.Waves {
			if !waves[w] {
				return fmt.Errorf("profile %q: wave %q is not in waves", name, w)
			}
		}
	}
	for name, p := range f.Profiles {
		if _, ok := f.Harnesses[p.Harness]; !ok {
			return fmt.Errorf("profile %q references unknown harness %q", name, p.Harness)
		}
		if p.Vendor == "" {
			return fmt.Errorf("profile %q: vendor is required (cross-vendor review rule)", name)
		}
	}
	if f.Routing.CrossVendorReview {
		var impl, rev []string
		for _, p := range f.Profiles {
			switch p.Role {
			case "implementer":
				impl = append(impl, p.Vendor)
			case "reviewer":
				rev = append(rev, p.Vendor)
			}
		}
		for _, iv := range impl {
			ok := false
			for _, rv := range rev {
				if rv != iv {
					ok = true
				}
			}
			if !ok {
				return fmt.Errorf("cross_vendor_review: no reviewer with vendor != %q", iv)
			}
		}
	}
	return nil
}

// SecretsFile is the only place fleet reads secrets from besides the process env and
// profile env files. fleet.yaml references them as ${VAR}.
const SecretsFile = "~/.config/fleet/env"

// ExpandPath replaces a leading ~ with the user's home directory.
func ExpandPath(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, p[1:])
}

// ParseEnvFile reads KEY=VALUE lines. Blank lines, # comments and a leading
// "export " are allowed; one layer of matching single or double quotes is stripped.
// A missing file is not an error — bootstrap creates it empty.
func ParseEnvFile(path string) (map[string]string, error) {
	b, err := os.ReadFile(ExpandPath(path))
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	env := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("%s:%d: expected KEY=VALUE", path, n)
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		env[strings.TrimSpace(k)] = v
	}
	return env, sc.Err()
}

// Lookup resolves ${VAR} references. fileEnv is the parsed SecretsFile.
type Lookup func(name string) (string, bool)

// SecretsLookup builds the Lookup used at runtime from SecretsFile and the process env.
func SecretsLookup() (Lookup, error) {
	fileEnv, err := ParseEnvFile(SecretsFile)
	if err != nil {
		return nil, err
	}
	return func(name string) (string, bool) { return resolveVar(name, fileEnv) }, nil
}

// resolveVar decides where a ${VAR} value comes from when both the secrets file
// and the process environment could supply it.
//
// The secrets file wins: the box then behaves identically over SSH and under systemd, and a
// stale export in a shell profile can't shadow a key rotated after `fleet panic`.
// An empty value in the file counts as unset, so a blank placeholder falls through to the
// environment and, failing that, is reported as missing rather than expanding to "".
func resolveVar(name string, fileEnv map[string]string) (string, bool) {
	if v := fileEnv[name]; v != "" {
		return v, true
	}
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return v, true
	}
	return "", false
}

// ExpandEnv returns a copy of m with ${VAR} / $VAR references resolved via lookup.
// Every unresolved name is reported in one error so a missing secret is caught
// before any command runs, not as an empty auth token mid-session.
func ExpandEnv(m map[string]string, lookup Lookup) (map[string]string, error) {
	out := make(map[string]string, len(m))
	missing := map[string]bool{}
	for k, v := range m {
		out[k] = os.Expand(v, func(name string) string {
			val, ok := lookup(name)
			if !ok {
				missing[name] = true
			}
			return val
		})
	}
	if len(missing) > 0 {
		names := make([]string, 0, len(missing))
		for n := range missing {
			names = append(names, n)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("unset variables %s — add them to %s", strings.Join(names, ", "), SecretsFile)
	}
	return out, nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

var versionRE = regexp.MustCompile(`\d+(?:\.\d+)+`)

// ParseVersion extracts the first dotted version number from s, so it accepts both
// "2.1.163" and CLI output such as "2.1.273 (Claude Code)" or "v1.18.18".
func ParseVersion(s string) ([]int, error) {
	m := versionRE.FindString(s)
	if m == "" {
		return nil, fmt.Errorf("no version number in %q", s)
	}
	var v []int
	for _, part := range strings.Split(m, ".") {
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("version %q: %w", m, err)
		}
		v = append(v, n)
	}
	return v, nil
}

// VersionAtLeast reports whether have >= min. Missing trailing parts count as 0.
func VersionAtLeast(have, min []int) bool {
	for i := 0; i < len(have) || i < len(min); i++ {
		var h, m int
		if i < len(have) {
			h = have[i]
		}
		if i < len(min) {
			m = min[i]
		}
		if h != m {
			return h > m
		}
	}
	return true
}

const (
	RoleImplementer = "implementer"
	RoleReviewer    = "reviewer"
	RoleFixer       = "fixer"
)

// ProfilesWhere returns the names of profiles matching keep, sorted, so every
// caller that picks "the profile for X" does so deterministically.
func (f *Fleet) ProfilesWhere(keep func(name string, p Profile) bool) []string {
	var out []string
	for name, p := range f.Profiles {
		if keep(name, p) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// WaveLabel is the GitHub label for a wave: its explicit label, else wave:<name>.
func (w Wave) WaveLabel() string {
	if w.Label != "" {
		return w.Label
	}
	return "wave:" + w.Name
}

// WaveForLabel maps a GitHub label back to a wave name, or "".
func (f *Fleet) WaveForLabel(label string) string {
	for _, w := range f.Waves {
		if w.WaveLabel() == label {
			return w.Name
		}
	}
	return ""
}
