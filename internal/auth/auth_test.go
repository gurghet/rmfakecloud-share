package auth

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"hash"
	"testing"
	"time"
)

const testSecret = "synthetic-test-secret-not-real"

var b64 = base64.RawURLEncoding

func rmfcKey(t *testing.T, secret string) []byte {
	t.Helper()
	k, err := pbkdf2.Key(sha256.New, secret, []byte("todo some salt"), 10000, 32)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func sign(t *testing.T, header, claims map[string]any, key []byte, h func() hash.Hash) string {
	t.Helper()
	hb, _ := json.Marshal(header)
	cb, _ := json.Marshal(claims)
	signing := b64.EncodeToString(hb) + "." + b64.EncodeToString(cb)
	m := hmac.New(h, key)
	m.Write([]byte(signing))
	return signing + "." + b64.EncodeToString(m.Sum(nil))
}

func hs256Header() map[string]any {
	return map[string]any{"alg": "HS256", "kid": "1", "typ": "JWT"}
}

func goodClaims(now time.Time) map[string]any {
	return map[string]any{
		"sub":       "user-1",
		"scopes":    "intgr screenshare docedit hwcmail:-1 hwc mail:-1 sync:tortoise share-url",
		"exp":       now.Add(time.Hour).Unix(),
		"nbf":       now.Add(-time.Minute).Unix(),
		"iat":       now.Add(-time.Minute).Unix(),
		"device-id": "synthetic-device",
		"level":     "connect",
		"iss":       "rM WebApp",
	}
}

func TestAcceptsRmfakecloudDerivedKey(t *testing.T) {
	now := time.Now()
	tok := sign(t, hs256Header(), goodClaims(now), rmfcKey(t, testSecret), sha256.New)
	v, _ := NewVerifier(testSecret, KeyModeRmfakecloud)
	c, err := v.Verify(tok, now)
	if err != nil {
		t.Fatalf("expected token to verify, got %v", err)
	}
	if c.Subject != "user-1" {
		t.Fatalf("subject = %q", c.Subject)
	}
}

func TestRejectsRawSecretSignedTokenInRmfakecloudMode(t *testing.T) {
	now := time.Now()
	tok := sign(t, hs256Header(), goodClaims(now), []byte(testSecret), sha256.New)
	v, _ := NewVerifier(testSecret, KeyModeRmfakecloud)
	if _, err := v.Verify(tok, now); err == nil {
		t.Fatal("token signed with the raw secret must be rejected in rmfakecloud mode")
	}
}

func TestRawModeAcceptsRawRejectsDerived(t *testing.T) {
	now := time.Now()
	v, _ := NewVerifier(testSecret, KeyModeRaw)
	raw := sign(t, hs256Header(), goodClaims(now), []byte(testSecret), sha256.New)
	if _, err := v.Verify(raw, now); err != nil {
		t.Fatalf("raw-signed token should verify in raw mode: %v", err)
	}
	der := sign(t, hs256Header(), goodClaims(now), rmfcKey(t, testSecret), sha256.New)
	if _, err := v.Verify(der, now); err == nil {
		t.Fatal("derived-key token must be rejected in raw mode")
	}
}

func TestRejections(t *testing.T) {
	now := time.Now()
	key := rmfcKey(t, testSecret)
	v, _ := NewVerifier(testSecret, KeyModeRmfakecloud)

	withClaim := func(k string, val any) map[string]any {
		c := goodClaims(now)
		c[k] = val
		return c
	}
	noneHeader := map[string]any{"alg": "none", "typ": "JWT"}
	noneTok := b64.EncodeToString(mustJSON(noneHeader)) + "." + b64.EncodeToString(mustJSON(goodClaims(now))) + "."

	cases := map[string]string{
		"alg none":          noneTok,
		"HS512":             sign(t, map[string]any{"alg": "HS512", "typ": "JWT"}, goodClaims(now), key, sha512.New),
		"expired":           sign(t, hs256Header(), withClaim("exp", now.Add(-time.Hour).Unix()), key, sha256.New),
		"not yet valid":     sign(t, hs256Header(), withClaim("nbf", now.Add(time.Hour).Unix()), key, sha256.New),
		"missing scope":     sign(t, hs256Header(), withClaim("scopes", "intgr docedit sync:tortoise"), key, sha256.New),
		"scope substring":   sign(t, hs256Header(), withClaim("scopes", "intgr share-url-extra"), key, sha256.New),
		"scopes not string": sign(t, hs256Header(), withClaim("scopes", []string{"share-url"}), key, sha256.New),
		"no exp": func() string {
			c := goodClaims(now)
			delete(c, "exp")
			return sign(t, hs256Header(), c, key, sha256.New)
		}(),
		"garbage":   "not-a-jwt",
		"empty":     "",
		"bad sig":   sign(t, hs256Header(), goodClaims(now), key, sha256.New) + "x",
		"two parts": "aaa.bbb",
	}
	for name, tok := range cases {
		if _, err := v.Verify(tok, now); err == nil {
			t.Errorf("%s: token must be rejected", name)
		}
	}
}

func TestClockSkewAllowance(t *testing.T) {
	now := time.Now()
	key := rmfcKey(t, testSecret)
	v, _ := NewVerifier(testSecret, KeyModeRmfakecloud)
	c := goodClaims(now)
	c["exp"] = now.Add(-10 * time.Second).Unix()
	c["nbf"] = now.Add(10 * time.Second).Unix()
	tok := sign(t, hs256Header(), c, key, sha256.New)
	if _, err := v.Verify(tok, now); err != nil {
		t.Fatalf("10s skew should be tolerated: %v", err)
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
