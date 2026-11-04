package config

import (
	"strings"
	"testing"
	"time"
)

func TestNewDefaults(t *testing.T) {
	c, err := Load(env(valid()))
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxPageBytes != 5_000_000 {
		t.Errorf("MaxPageBytes = %d", c.MaxPageBytes)
	}
	if c.MaxConcurrentUploads != 2 || c.MaxPagesPerDay != 200 || c.RetentionDays != 0 || c.ShutdownGrace != 90*time.Second {
		t.Errorf("defaults wrong: %+v", c)
	}
}

func TestNewOverrides(t *testing.T) {
	m := valid()
	m["RMSHARE_MAX_PAGE_BYTES"] = "1000"
	m["RMSHARE_MAX_CONCURRENT_UPLOADS"] = "4"
	m["RMSHARE_MAX_PAGES_PER_DAY"] = "0"
	m["RMSHARE_RETENTION_DAYS"] = "30"
	m["RMSHARE_SHUTDOWN_GRACE"] = "5m"
	c, err := Load(env(m))
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxPageBytes != 1000 || c.MaxConcurrentUploads != 4 || c.MaxPagesPerDay != 0 || c.RetentionDays != 30 || c.ShutdownGrace != 5*time.Minute {
		t.Fatalf("overrides not applied: %+v", c)
	}
}

func TestPublicURLMustEndWithShare(t *testing.T) {
	for _, u := range []string{"https://notes.example.com", "https://notes.example.com/other", "https://notes.example.com/share/x"} {
		m := valid()
		m["RMSHARE_PUBLIC_URL"] = u
		_, err := Load(env(m))
		if err == nil || !strings.Contains(err.Error(), "/share") {
			t.Errorf("%s: err = %v, want error mentioning /share", u, err)
		}
	}
	m := valid()
	m["RMSHARE_PUBLIC_URL"] = "https://lan.example.com:8443/share"
	if _, err := Load(env(m)); err != nil {
		t.Errorf("valid url rejected: %v", err)
	}
}

func TestNewInvalidValues(t *testing.T) {
	for k, v := range map[string]string{
		"RMSHARE_MAX_PAGE_BYTES": "0", "RMSHARE_MAX_CONCURRENT_UPLOADS": "0", "RMSHARE_MAX_PAGES_PER_DAY": "-1",
		"RMSHARE_RETENTION_DAYS": "-3", "RMSHARE_SHUTDOWN_GRACE": "soon",
	} {
		m := valid()
		m[k] = v
		if _, err := Load(env(m)); err == nil {
			t.Errorf("%s=%q should be rejected", k, v)
		}
	}
}
