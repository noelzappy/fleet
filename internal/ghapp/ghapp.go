// Package ghapp holds the pure parts of running a fleet on a GitHub App instead of a
// personal login: the App manifest, the JWT that authenticates as the App, the token
// cache, and the git credential-helper output. Network calls live in internal/cli and
// go through internal/shell like every other side effect.
package ghapp

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/noelzappy/fleet/internal/config"
)

// Permissions are everything agents and fleet need, and nothing more. Deliberately
// absent: administration (branch protection, settings) and workflows (CI files); a
// token without them can't weaken the checks that gate a merge.
var Permissions = map[string]string{
	"contents":      "write", // push branches
	"pull_requests": "write", // open PRs, post reviews
	"issues":        "write", // labels, comments, pause issues
	"metadata":      "read",
	"checks":        "read", // gate status
	"actions":       "read", // gate runs and logs for sync
	"statuses":      "read", // commit statuses on PRs
}

// Forbidden are permissions whose presence on a token fails `fleet github app use`.
var Forbidden = []string{"administration", "workflows", "secrets", "environments", "repository_hooks"}

// Manifest is the GitHub App manifest posted to github.com/settings/apps/new.
func Manifest(name, repo, redirectURL string) map[string]any {
	return map[string]any{
		"name":                name,
		"url":                 "https://github.com/" + repo,
		"description":         "fleet: coding agents for " + repo + ". Scoped: no administration, no workflows.",
		"public":              false,
		"redirect_url":        redirectURL,
		"default_permissions": Permissions,
		"default_events":      []string{},
		"hook_attributes":     map[string]any{"url": "https://github.com/" + repo, "active": false},
	}
}

// NewURL is where the manifest form posts: the user's or the organization's App settings.
func NewURL(ownerType, owner, state string) string {
	if strings.EqualFold(ownerType, "Organization") {
		return "https://github.com/organizations/" + owner + "/settings/apps/new?state=" + state
	}
	return "https://github.com/settings/apps/new?state=" + state
}

// JWT signs the App JWT: RS256, iss = client ID, iat 60s in the past, exp 9 min ahead.
func JWT(clientID string, pemBytes []byte, now time.Time) (string, error) {
	key, err := parseKey(pemBytes)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": clientID,
	})
	signing := enc.EncodeToString(header) + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

func parseKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("private key: no PEM block")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("private key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private key: not RSA")
	}
	return rk, nil
}

// State is what `fleet github app create` learns and later commands need. It lives in
// ~/.config/fleet/gh-app.json (0600), not in fleet.yaml: it's generated, not configured.
type State struct {
	ID             int64  `json:"id"`
	ClientID       string `json:"client_id"`
	Slug           string `json:"slug"`
	HTMLURL        string `json:"html_url"`
	InstallationID int64  `json:"installation_id,omitempty"`
	RealGH         string `json:"real_gh,omitempty"` // the gh binary behind fleet's wrapper
}

var (
	StatePath = config.ExpandPath("~/.config/fleet/gh-app.json")
	TokenPath = config.ExpandPath("~/.config/fleet/gh-app-token.json")
	// WrapperDir holds the `gh` wrapper that puts an App token in GH_TOKEN. It goes first
	// on PATH for login shells, so agents and fleet's own gh calls both use it.
	WrapperDir = config.ExpandPath("~/.local/share/fleet/bin")
)

func LoadState() (State, error) {
	var s State
	b, err := os.ReadFile(StatePath)
	if err != nil {
		if os.IsNotExist(err) {
			return s, fmt.Errorf("no GitHub App yet: run `fleet github app create`")
		}
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("%s: %w", StatePath, err)
	}
	return s, nil
}

func (s State) JSON() []byte {
	b, _ := json.MarshalIndent(s, "", "  ")
	return append(b, '\n')
}

// Token is a cached installation access token.
type Token struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Fresh reports whether the token has at least 10 minutes left, so an agent never starts
// a push with a token about to expire.
func (t Token) Fresh(now time.Time) bool {
	return t.Token != "" && t.ExpiresAt.After(now.Add(10*time.Minute))
}

// LoadToken returns the cached token, or a zero Token if there is none.
func LoadToken() Token {
	var t Token
	if b, err := os.ReadFile(TokenPath); err == nil {
		_ = json.Unmarshal(b, &t)
	}
	return t
}

// CredentialOutput is what a git credential helper prints for `get`.
func CredentialOutput(token string) string {
	return "username=x-access-token\npassword=" + token + "\n"
}

// Wrapper is the `gh` shim: every call gets a fresh App token.
func Wrapper(fleetBin, configPath, realGH string) []byte {
	return fmt.Appendf(nil, `#!/bin/sh
# GENERATED by fleet github app use — gh with a GitHub App installation token.
# The owner's personal login must not exist on this box; see README › GitHub App.
GH_TOKEN="$(%s -c %s github token)" || exit 1
export GH_TOKEN
exec %s "$@"
`, shq(fleetBin), shq(configPath), shq(realGH))
}

// DenyPermissions returns forbidden permissions present in a token's permission set.
func DenyPermissions(perms map[string]string) []string {
	var bad []string
	for _, p := range Forbidden {
		if _, ok := perms[p]; ok {
			bad = append(bad, p+":"+perms[p])
		}
	}
	return bad
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
