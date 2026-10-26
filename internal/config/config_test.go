package config

import (
	"encoding/base64"
	"strings"
	"testing"
)

var key32 = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))

func env(over map[string]string) func(string) string {
	m := map[string]string{
		"DISCORD_CLIENT_ID":     "123",
		"DISCORD_CLIENT_SECRET": "supersecret",
		"PUBLIC_URL":            "https://presence.example.com",
		"NAVIDROME_URL":         "http://navidrome:4533",
		"SHARED_SECRET":         "shh",
		"ENCRYPTION_KEY":        key32,
		"SESSION_KEY":           key32,
		"DB_PATH":               "/data/db.sqlite",
	}
	for k, v := range over {
		if v == "-" {
			delete(m, k)
		} else {
			m[k] = v
		}
	}
	return func(k string) string { return m[k] }
}

func TestConfigLoadsDefaults(t *testing.T) {
	c, err := Load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicAddr != ":8080" || c.InternalAddr != ":8081" || c.ClientID != "123" || len(c.EncryptionKey) != 32 || len(c.SessionKey) != 32 {
		t.Fatalf("%+v", c)
	}
}

func TestConfigMissingVar(t *testing.T) {
	for _, name := range []string{"DISCORD_CLIENT_ID", "DISCORD_CLIENT_SECRET", "PUBLIC_URL", "NAVIDROME_URL", "SHARED_SECRET", "ENCRYPTION_KEY", "SESSION_KEY", "DB_PATH"} {
		_, err := Load(env(map[string]string{name: "-"}))
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestConfigShortKey(t *testing.T) {
	short := base64.StdEncoding.EncodeToString([]byte("short"))
	for _, name := range []string{"ENCRYPTION_KEY", "SESSION_KEY"} {
		for _, v := range []string{short, "!!notbase64!!"} {
			_, err := Load(env(map[string]string{name: v}))
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Errorf("%s=%q: err = %v", name, v, err)
			}
		}
	}
}

func TestConfigErrorOmitsValues(t *testing.T) {
	_, err := Load(env(map[string]string{"DISCORD_CLIENT_SECRET": "supersecret", "ENCRYPTION_KEY": "bad-key-value", "PUBLIC_URL": "http://leak.example"}))
	if err == nil {
		t.Fatal("want error")
	}
	for _, v := range []string{"supersecret", "bad-key-value", "leak.example"} {
		if strings.Contains(err.Error(), v) {
			t.Fatalf("error leaks %q: %v", v, err)
		}
	}
}

func TestConfigPublicURL(t *testing.T) {
	for _, u := range []string{"http://a.example", "a.example", "https://", "https://a.example/app", "https://a.example/?q=1", "https://a.example/#f", "https://a.example:443", "https://a.example:443/", "https://user@a.example", "https://a.example?"} {
		if _, err := Load(env(map[string]string{"PUBLIC_URL": u})); err == nil {
			t.Errorf("%q accepted", u)
		}
	}
	for in, want := range map[string]string{"https://a.example": "https://a.example", "https://a.example/": "https://a.example", "https://a.example:8443/": "https://a.example:8443"} {
		c, err := Load(env(map[string]string{"PUBLIC_URL": in}))
		if err != nil || c.PublicURL != want {
			t.Errorf("%q: %q, %v", in, c.PublicURL, err)
		}
	}
}
