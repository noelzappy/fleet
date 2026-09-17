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
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestJWT(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	now := time.Unix(1_800_000_000, 0)
	tok, err := JWT("Iv23liClient", pemBytes, now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("parts = %d", len(parts))
	}
	enc := base64.RawURLEncoding
	var header map[string]string
	b, _ := enc.DecodeString(parts[0])
	_ = json.Unmarshal(b, &header)
	if header["alg"] != "RS256" {
		t.Errorf("alg = %s", header["alg"])
	}
	var claims map[string]any
	b, _ = enc.DecodeString(parts[1])
	_ = json.Unmarshal(b, &claims)
	if claims["iss"] != "Iv23liClient" || int64(claims["iat"].(float64)) != now.Unix()-60 || int64(claims["exp"].(float64)) > now.Unix()+600 {
		t.Errorf("claims = %v", claims)
	}
	sig, _ := enc.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Errorf("signature: %v", err)
	}

	pk8, _ := x509.MarshalPKCS8PrivateKey(key)
	if _, err := JWT("x", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk8}), now); err != nil {
		t.Errorf("PKCS8 key: %v", err)
	}
	if _, err := JWT("x", []byte("not a key"), now); err == nil {
		t.Error("garbage key accepted")
	}
}

func TestManifestIsScoped(t *testing.T) {
	m := Manifest("widgets-fleet", "o/widgets", "http://100.64.0.1:8787/callback")
	perms := m["default_permissions"].(map[string]string)
	if bad := DenyPermissions(perms); len(bad) != 0 {
		t.Errorf("manifest asks for forbidden permissions: %v", bad)
	}
	for _, p := range []string{"contents", "pull_requests", "issues", "actions"} {
		if perms[p] == "" {
			t.Errorf("missing %s", p)
		}
	}
	if m["public"] != false || m["redirect_url"] != "http://100.64.0.1:8787/callback" {
		t.Errorf("manifest = %v", m)
	}
	if got := DenyPermissions(map[string]string{"contents": "write", "administration": "write", "workflows": "write"}); len(got) != 2 {
		t.Errorf("DenyPermissions = %v", got)
	}
}

func TestNewURL(t *testing.T) {
	if got := NewURL("User", "noelzappy", "s1"); got != "https://github.com/settings/apps/new?state=s1" {
		t.Error(got)
	}
	if got := NewURL("Organization", "acme", "s1"); got != "https://github.com/organizations/acme/settings/apps/new?state=s1" {
		t.Error(got)
	}
}

func TestTokenFresh(t *testing.T) {
	now := time.Now()
	for _, tt := range []struct {
		tok  Token
		want bool
	}{
		{Token{}, false},
		{Token{Token: "t", ExpiresAt: now.Add(55 * time.Minute)}, true},
		{Token{Token: "t", ExpiresAt: now.Add(9 * time.Minute)}, false},
	} {
		if got := tt.tok.Fresh(now); got != tt.want {
			t.Errorf("Fresh(%v) = %v", tt.tok.ExpiresAt.Sub(now), got)
		}
	}
}

func TestCredentialAndWrapper(t *testing.T) {
	if CredentialOutput("ghs_x") != "username=x-access-token\npassword=ghs_x\n" {
		t.Error("credential output")
	}
	w := string(Wrapper("/home/f/.local/bin/fleet", "/home/f/it's/fleet.yaml", "/usr/bin/gh"))
	if !strings.Contains(w, `'/home/f/it'\''s/fleet.yaml'`) || !strings.Contains(w, "exec '/usr/bin/gh' \"$@\"") {
		t.Errorf("wrapper:\n%s", w)
	}
	if out, err := exec.Command("sh", "-n", "-c", w).CombinedOutput(); err != nil {
		t.Errorf("wrapper doesn't parse: %s", out)
	}
}
