package githubapp

import (
	"bytes"
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
	"strconv"
	"time"
)

type Clock interface{ Now() time.Time }
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now().UTC() }

type JWTSigner struct {
	AppID  int64
	KeyPEM []byte
	Clock  Clock
}

func (s JWTSigner) Sign() (string, error) {
	if s.AppID < 1 || s.Clock == nil {
		return "", errors.New("invalid GitHub App JWT signer configuration")
	}
	block, rest := pem.Decode(s.KeyPEM)
	if block == nil || (block.Type != "PRIVATE KEY" && block.Type != "RSA PRIVATE KEY") || len(bytes.TrimSpace(rest)) != 0 {
		return "", errors.New("invalid GitHub App private key format")
	}
	var key *rsa.PrivateKey
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err == nil {
		key, _ = parsed.(*rsa.PrivateKey)
	} else if k, e := x509.ParsePKCS1PrivateKey(block.Bytes); e == nil {
		key = k
	}
	if key == nil || key.N.BitLen() < 2048 {
		return "", errors.New("GitHub App private key must be RSA with at least 2048 bits")
	}
	if err := key.Validate(); err != nil {
		return "", errors.New("invalid GitHub App RSA private key")
	}
	now := s.Clock.Now().UTC()
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	claims := map[string]any{"iat": now.Add(-30 * time.Second).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": strconv.FormatInt(s.AppID, 10)}
	h, _ := json.Marshal(header)
	c, _ := json.Marshal(claims)
	enc := base64.RawURLEncoding.EncodeToString
	signing := enc(h) + "." + enc(c)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign GitHub App JWT: %w", err)
	}
	return signing + "." + enc(sig), nil
}
