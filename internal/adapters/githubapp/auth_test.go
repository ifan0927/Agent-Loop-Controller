package githubapp

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fixedClock struct{ value time.Time }

func (c fixedClock) Now() time.Time { return c.value }

func testKey(t *testing.T) ([]byte, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	raw := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: mustPKCS8(t, key)})
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "app.pem")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return raw, path
}
func mustPKCS8(t *testing.T, key *rsa.PrivateKey) []byte {
	t.Helper()
	b, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestJWTClaimsAndRS256(t *testing.T) {
	raw, _ := testKey(t)
	now := time.Date(2026, 7, 11, 1, 2, 3, 0, time.UTC)
	token, err := (JWTSigner{AppID: 42, KeyPEM: raw, Clock: fixedClock{now}}).Sign()
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatal("JWT segments")
	}
	var header map[string]any
	h, _ := base64.RawURLEncoding.DecodeString(parts[0])
	json.Unmarshal(h, &header)
	if header["alg"] != "RS256" {
		t.Fatalf("alg=%v", header["alg"])
	}
	var claims map[string]any
	c, _ := base64.RawURLEncoding.DecodeString(parts[1])
	json.Unmarshal(c, &claims)
	if claims["iss"] != "42" || int64(claims["iat"].(float64)) != now.Add(-30*time.Second).Unix() || int64(claims["exp"].(float64)) != now.Add(9*time.Minute).Unix() {
		t.Fatalf("claims=%v", claims)
	}
}

func TestPrivateKeyBoundary(t *testing.T) {
	_, path := testKey(t)
	cfg := validConfig(path)
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "key.pem")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	cfg.PrivateKeyFile = link
	if err := cfg.Validate(); err == nil {
		t.Fatal("symlink key accepted")
	}
	bad := filepath.Join(t.TempDir(), "bad.pem")
	os.WriteFile(bad, []byte("not a key"), 0o600)
	if _, err := (JWTSigner{AppID: 1, KeyPEM: []byte("not a key"), Clock: fixedClock{time.Now()}}).Sign(); err == nil {
		t.Fatal("malformed key accepted")
	}
}

func TestUnavailablePrivateKeyDoesNotExposePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private-key-not-for-output.pem")
	_, err := ReadPrivateKeyFile(path)
	if err == nil || strings.Contains(err.Error(), path) || strings.Contains(err.Error(), "not-for-output") {
		t.Fatalf("credential error leaked path: %v", err)
	}
}

func validConfig(path string) Config {
	return Config{APIBaseURL: "https://api.github.com", GraphQLURL: "https://api.github.com/graphql", AppID: 1, InstallationID: 2, RepositoryOwner: "owner", RepositoryName: "repo", RepositoryID: 3, PrivateKeyFile: path, HTTPTimeout: time.Second, TokenRefreshSkew: time.Minute, APIVersion: "2022-11-28"}
}
