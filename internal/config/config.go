// Package config loads settings from environment variables.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gurghet/rmfakecloud-share/internal/auth"
)

// Config holds all runtime settings.
type Config struct {
	Listen       string
	PublicURL    string // no trailing slash
	DataDir      string
	JWTSecret    string
	JWTKeyMode   auth.KeyMode
	AnthropicKey string
	Model        string // empty = converter default
	MaxBodyBytes int64
	MaxPages     int

	MaxPageBytes         int64
	MaxConcurrentUploads int
	MaxPagesPerDay       int // 0 = unlimited
	RetentionDays        int // 0 = keep forever
	ShutdownGrace        time.Duration
}

// Load reads configuration using getenv (os.Getenv in production).
func Load(getenv func(string) string) (Config, error) {
	var errs []error
	get := func(k string) string { return strings.TrimSpace(getenv(k)) }
	required := func(k string) string {
		v := get(k)
		if v == "" {
			errs = append(errs, fmt.Errorf("%s is required", k))
		}
		return v
	}
	c := Config{
		Listen:       get("RMSHARE_LISTEN"),
		PublicURL:    required("RMSHARE_PUBLIC_URL"),
		DataDir:      required("RMSHARE_DATA_DIR"),
		JWTSecret:    getenv("JWT_SECRET_KEY"), // not trimmed: rmfakecloud uses the value as-is
		AnthropicKey: get("ANTHROPIC_API_KEY"),
		Model:        get("RMSHARE_MODEL"),
		MaxBodyBytes: 20 << 20,
		MaxPages:     50,

		MaxPageBytes:         5_000_000, // raw bytes; ~6.7 MB as base64, under the Claude API's 10 MB base64 per-image limit
		MaxConcurrentUploads: 2,
		MaxPagesPerDay:       200,
		ShutdownGrace:        90 * time.Second,
	}
	if c.JWTSecret == "" {
		errs = append(errs, errors.New("JWT_SECRET_KEY is required (same value as rmfakecloud)"))
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:3004"
	}
	if c.PublicURL != "" {
		c.PublicURL = strings.TrimRight(c.PublicURL, "/")
		if !strings.HasSuffix(c.PublicURL, "/share") {
			errs = append(errs, fmt.Errorf("RMSHARE_PUBLIC_URL must end with /share (the pages are served under /share/), got %q", c.PublicURL))
		}
		u, err := url.Parse(c.PublicURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("RMSHARE_PUBLIC_URL must be an absolute http(s) URL, got %q", c.PublicURL))
		}
	}
	mode, err := auth.ParseKeyMode(get("RMSHARE_JWT_KEY_MODE"))
	if err != nil {
		errs = append(errs, fmt.Errorf("RMSHARE_JWT_KEY_MODE: %w", err))
	}
	c.JWTKeyMode = mode
	if v := get("RMSHARE_MAX_BODY_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			errs = append(errs, fmt.Errorf("RMSHARE_MAX_BODY_BYTES must be a positive integer, got %q", v))
		} else {
			c.MaxBodyBytes = n
		}
	}
	if v := get("RMSHARE_MAX_PAGES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			errs = append(errs, fmt.Errorf("RMSHARE_MAX_PAGES must be a positive integer, got %q", v))
		} else {
			c.MaxPages = n
		}
	}
	intVar := func(name string, dst *int, min int) {
		if v := get(name); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < min {
				errs = append(errs, fmt.Errorf("%s must be an integer >= %d, got %q", name, min, v))
			} else {
				*dst = n
			}
		}
	}
	if v := get("RMSHARE_MAX_PAGE_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			errs = append(errs, fmt.Errorf("RMSHARE_MAX_PAGE_BYTES must be a positive integer, got %q", v))
		} else {
			c.MaxPageBytes = n
		}
	}
	intVar("RMSHARE_MAX_CONCURRENT_UPLOADS", &c.MaxConcurrentUploads, 1)
	intVar("RMSHARE_MAX_PAGES_PER_DAY", &c.MaxPagesPerDay, 0)
	intVar("RMSHARE_RETENTION_DAYS", &c.RetentionDays, 0)
	if v := get("RMSHARE_SHUTDOWN_GRACE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			errs = append(errs, fmt.Errorf("RMSHARE_SHUTDOWN_GRACE must be a duration like 90s, got %q", v))
		} else {
			c.ShutdownGrace = d
		}
	}
	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return c, nil
}
