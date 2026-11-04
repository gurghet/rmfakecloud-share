// Package config loads settings from environment variables.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

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
		MaxBodyBytes: 64 << 20,
		MaxPages:     50,
	}
	if c.JWTSecret == "" {
		errs = append(errs, errors.New("JWT_SECRET_KEY is required (same value as rmfakecloud)"))
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:3004"
	}
	if c.PublicURL != "" {
		c.PublicURL = strings.TrimRight(c.PublicURL, "/")
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
	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return c, nil
}
