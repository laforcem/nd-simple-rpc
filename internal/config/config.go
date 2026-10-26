// Package config loads the companion's configuration from the environment.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Config is the validated configuration.
type Config struct {
	ClientID      string
	ClientSecret  string
	PublicURL     string // no trailing slash
	NavidromeURL  string
	SharedSecret  string
	EncryptionKey []byte
	SessionKey    []byte
	DBPath        string
	PublicAddr    string
	InternalAddr  string
	// TrustedProxyHops is how many reverse proxies append to X-Forwarded-For
	// in front of the public port; 0 ignores the header.
	TrustedProxyHops int
}

// Load reads and validates the configuration. Errors name variables, never
// their values.
func Load(getenv func(string) string) (Config, error) {
	var c Config
	var errs []error
	req := func(name string) string {
		v := getenv(name)
		if v == "" {
			errs = append(errs, fmt.Errorf("%s is required", name))
		}
		return v
	}
	key := func(name string) []byte {
		v := req(name)
		if v == "" {
			return nil
		}
		b, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s is not valid base64", name))
			return nil
		}
		if len(b) != 32 {
			errs = append(errs, fmt.Errorf("%s must decode to 32 bytes", name))
			return nil
		}
		return b
	}
	opt := func(name, def string) string {
		if v := getenv(name); v != "" {
			return v
		}
		return def
	}

	c.ClientID = req("DISCORD_CLIENT_ID")
	c.ClientSecret = req("DISCORD_CLIENT_SECRET")
	c.NavidromeURL = req("NAVIDROME_URL")
	c.SharedSecret = req("SHARED_SECRET")
	c.DBPath = req("DB_PATH")
	c.EncryptionKey = key("ENCRYPTION_KEY")
	c.SessionKey = key("SESSION_KEY")
	c.PublicAddr = opt("PUBLIC_ADDR", ":8080")
	c.InternalAddr = opt("INTERNAL_ADDR", ":8081")
	c.TrustedProxyHops = 1
	if v := getenv("TRUSTED_PROXY_HOPS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 5 {
			errs = append(errs, errors.New("TRUSTED_PROXY_HOPS must be an integer from 0 to 5"))
		}
		c.TrustedProxyHops = n
	}
	if v := req("PUBLIC_URL"); v != "" {
		u, err := parsePublicURL(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("PUBLIC_URL %w", err))
		}
		c.PublicURL = u
	}
	if err := errors.Join(errs...); err != nil {
		return Config{}, err
	}
	return c, nil
}

func parsePublicURL(s string) (string, error) {
	u, err := url.Parse(s)
	if err != nil {
		return "", errors.New("is not a valid URL")
	}
	switch {
	case u.Scheme != "https":
		return "", errors.New("must use https")
	case u.Hostname() == "":
		return "", errors.New("must have a host")
	case u.User != nil:
		return "", errors.New("must not contain credentials")
	case u.Path != "" && u.Path != "/":
		return "", errors.New("must not have a path")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(s, "#"):
		return "", errors.New("must not have a query or fragment")
	case u.Port() == "443":
		return "", errors.New("must not include the default port :443")
	}
	return strings.TrimSuffix(s, "/"), nil
}
