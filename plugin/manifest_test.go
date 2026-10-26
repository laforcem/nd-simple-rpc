package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Navidrome matches requiredHosts only as an exact host, "*", a leading
// "*." wildcard, or an IP/CIDR; any other wildcard silently never matches.
func TestManifestRequiredHostsMatchable(t *testing.T) {
	b, err := os.ReadFile("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Permissions struct {
			HTTP struct {
				RequiredHosts []string `json:"requiredHosts"`
			} `json:"http"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	hosts := m.Permissions.HTTP.RequiredHosts
	if len(hosts) == 0 {
		t.Fatal("no requiredHosts")
	}
	for _, h := range hosts {
		rest := strings.TrimPrefix(h, "*.")
		if h != "*" && strings.Contains(rest, "*") {
			t.Errorf("requiredHosts entry %q uses an unsupported wildcard", h)
		}
	}
}
