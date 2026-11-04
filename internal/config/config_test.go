package config

import (
	"strings"
	"testing"

	"github.com/gurghet/rmfakecloud-share/internal/auth"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func valid() map[string]string {
	return map[string]string{
		"RMSHARE_PUBLIC_URL": "https://notes.example.com/share/",
		"RMSHARE_DATA_DIR":   "/var/lib/rmshare",
		"JWT_SECRET_KEY":     "s3cret",
	}
}

func TestDefaultsAndNormalisation(t *testing.T) {
	c, err := Load(env(valid()))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:3004" {
		t.Errorf("Listen = %q", c.Listen)
	}
	if c.PublicURL != "https://notes.example.com/share" {
		t.Errorf("PublicURL must lose its trailing slash, got %q", c.PublicURL)
	}
	if c.JWTKeyMode != auth.KeyModeRmfakecloud {
		t.Errorf("JWTKeyMode = %q", c.JWTKeyMode)
	}
	if c.MaxBodyBytes != 64<<20 || c.MaxPages != 50 {
		t.Errorf("limits = %d, %d", c.MaxBodyBytes, c.MaxPages)
	}
	if c.AnthropicKey != "" {
		t.Error("API key must default to empty (conversion disabled)")
	}
}

func TestOverrides(t *testing.T) {
	m := valid()
	m["RMSHARE_LISTEN"] = "0.0.0.0:9000"
	m["RMSHARE_JWT_KEY_MODE"] = "raw"
	m["RMSHARE_MAX_BODY_BYTES"] = "1024"
	m["RMSHARE_MAX_PAGES"] = "3"
	m["ANTHROPIC_API_KEY"] = "k"
	m["RMSHARE_MODEL"] = "some-model"
	c, err := Load(env(m))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "0.0.0.0:9000" || c.JWTKeyMode != auth.KeyModeRaw || c.MaxBodyBytes != 1024 || c.MaxPages != 3 || c.AnthropicKey != "k" || c.Model != "some-model" {
		t.Fatalf("overrides not applied: %+v", c)
	}
}

func TestMissingRequiredFailsFastNamingTheVariable(t *testing.T) {
	for _, k := range []string{"RMSHARE_PUBLIC_URL", "RMSHARE_DATA_DIR", "JWT_SECRET_KEY"} {
		m := valid()
		delete(m, k)
		_, err := Load(env(m))
		if err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("missing %s: err = %v, want error naming it", k, err)
		}
	}
}

func TestInvalidValues(t *testing.T) {
	cases := map[string]string{
		"RMSHARE_JWT_KEY_MODE":   "md5",
		"RMSHARE_MAX_BODY_BYTES": "-1",
		"RMSHARE_MAX_PAGES":      "abc",
		"RMSHARE_PUBLIC_URL":     "not a url",
	}
	for k, v := range cases {
		m := valid()
		m[k] = v
		if _, err := Load(env(m)); err == nil {
			t.Errorf("%s=%q should be rejected", k, v)
		}
	}
	m := valid()
	m["RMSHARE_PUBLIC_URL"] = "ftp://x.example.com"
	if _, err := Load(env(m)); err == nil {
		t.Error("non-http(s) public URL should be rejected")
	}
}
