// nd-simple-rpc Navidrome plugin: enriches playback reports and forwards them
// to the companion service. It holds no state.
package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/navidrome/navidrome/plugins/pdk/go/host"
	"github.com/navidrome/navidrome/plugins/pdk/go/pdk"
	"github.com/navidrome/navidrome/plugins/pdk/go/scrobbler"
)

const (
	companionURLKey  = "companionUrl"
	sharedSecretKey  = "sharedSecret"
	playbackPath     = "/internal/v1/playback"
	forwardTimeoutMs = 3000
)

type plugin struct{}

func init() {
	scrobbler.Register(&plugin{})
}

func (*plugin) IsAuthorized(scrobbler.IsAuthorizedRequest) (bool, error) { return true, nil }
func (*plugin) NowPlaying(scrobbler.NowPlayingRequest) error             { return nil }
func (*plugin) Scrobble(scrobbler.ScrobbleRequest) error                 { return nil }

// PlaybackReport forwards the enriched event. Failures are logged (never the
// URL, secret or cover URL) and swallowed so Navidrome is not affected.
func (*plugin) PlaybackReport(req scrobbler.PlaybackReportRequest) error {
	base, _ := pdk.GetConfig(companionURLKey)
	secret, _ := pdk.GetConfig(sharedSecretKey)
	if base == "" || secret == "" {
		pdk.Log(pdk.LogWarn, "companionUrl or sharedSecret not configured")
		return nil
	}

	body, err := json.Marshal(BuildEvent(req, navidromeHost{}))
	if err != nil {
		pdk.Log(pdk.LogError, fmt.Sprintf("marshal event: %v", err))
		return nil
	}

	resp, err := host.HTTPSend(host.HTTPRequest{
		Method: "POST",
		URL:    strings.TrimRight(base, "/") + playbackPath,
		Headers: map[string]string{
			"Content-Type":  "application/json",
			"Authorization": "Bearer " + secret,
		},
		Body:      body,
		TimeoutMs: forwardTimeoutMs,
	})
	if err != nil {
		pdk.Log(pdk.LogError, "forward playback event failed (request error)")
		return nil
	}
	if resp.StatusCode >= 300 {
		pdk.Log(pdk.LogError, fmt.Sprintf("companion rejected playback event: HTTP %d", resp.StatusCode))
	}
	return nil
}

func main() {}
