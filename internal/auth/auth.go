// Package auth verifies rmfakecloud user JWTs.
//
// Only HS256 is accepted. rmfakecloud signs user tokens with a key derived from
// JWT_SECRET_KEY via PBKDF2 (see docs/protocol.md), not with the raw value.
package auth

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// KeyMode selects how the HMAC key is derived from JWT_SECRET_KEY.
type KeyMode string

const (
	// KeyModeRmfakecloud derives the key with PBKDF2 like rmfakecloud does.
	KeyModeRmfakecloud KeyMode = "rmfakecloud"
	// KeyModeRaw uses the secret bytes as-is.
	KeyModeRaw KeyMode = "raw"
)

// RequiredScope must be present in the token's scopes claim.
const RequiredScope = "share-url"

// clockSkew is the tolerance applied to exp and nbf.
const clockSkew = 30 * time.Second

// ErrInvalid is returned (wrapped) for every verification failure.
var ErrInvalid = errors.New("invalid token")

// Claims are the verified claims we care about.
type Claims struct {
	Subject string
	Scopes  []string
}

// Verifier checks tokens.
type Verifier struct {
	key []byte
}

// ParseKeyMode validates a configuration string.
func ParseKeyMode(s string) (KeyMode, error) {
	switch KeyMode(s) {
	case "", KeyModeRmfakecloud:
		return KeyModeRmfakecloud, nil
	case KeyModeRaw:
		return KeyModeRaw, nil
	}
	return "", fmt.Errorf("unknown key mode %q (want %q or %q)", s, KeyModeRmfakecloud, KeyModeRaw)
}

// NewVerifier builds a Verifier from the shared secret.
func NewVerifier(secret string, mode KeyMode) (*Verifier, error) {
	if secret == "" {
		return nil, errors.New("empty JWT secret")
	}
	switch mode {
	case KeyModeRaw:
		return &Verifier{key: []byte(secret)}, nil
	case KeyModeRmfakecloud, "":
		// Mirrors rmfakecloud internal/config: pbkdf2(secret, "todo some salt", 10000, 32, sha256).
		k, err := pbkdf2.Key(sha256.New, secret, []byte("todo some salt"), 10000, 32)
		if err != nil {
			return nil, err
		}
		return &Verifier{key: k}, nil
	}
	return nil, fmt.Errorf("unknown key mode %q", mode)
}

type header struct {
	Alg string `json:"alg"`
}

type rawClaims struct {
	Sub    string   `json:"sub"`
	Scopes any      `json:"scopes"`
	Exp    *float64 `json:"exp"`
	Nbf    *float64 `json:"nbf"`
}

func fail(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// Verify validates token at time now and returns its claims.
func (v *Verifier) Verify(token string, now time.Time) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, fail("not a compact JWT")
	}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Claims{}, fail("bad header encoding")
	}
	var h header
	if err := json.Unmarshal(hb, &h); err != nil {
		return Claims{}, fail("bad header")
	}
	if h.Alg != "HS256" {
		return Claims{}, fail("unsupported alg")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, fail("bad signature encoding")
	}
	mac := hmac.New(sha256.New, v.key)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return Claims{}, fail("signature mismatch")
	}
	cb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, fail("bad claims encoding")
	}
	var rc rawClaims
	if err := json.Unmarshal(cb, &rc); err != nil {
		return Claims{}, fail("bad claims")
	}
	if rc.Exp == nil {
		return Claims{}, fail("missing exp")
	}
	if now.After(time.Unix(int64(*rc.Exp), 0).Add(clockSkew)) {
		return Claims{}, fail("expired")
	}
	if rc.Nbf != nil && now.Add(clockSkew).Before(time.Unix(int64(*rc.Nbf), 0)) {
		return Claims{}, fail("not yet valid")
	}
	scopes, ok := rc.Scopes.(string)
	if !ok {
		return Claims{}, fail("scopes claim missing or not a string")
	}
	fields := strings.Fields(scopes)
	has := false
	for _, s := range fields {
		if s == RequiredScope {
			has = true
		}
	}
	if !has {
		return Claims{}, fail("missing %s scope", RequiredScope)
	}
	return Claims{Subject: rc.Sub, Scopes: fields}, nil
}
