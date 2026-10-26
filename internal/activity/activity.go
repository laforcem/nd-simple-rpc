// Package activity maps playback events to Discord activity payloads.
package activity

import (
	"time"

	"github.com/laforcem/nd-simple-rpc/internal/event"
)

// maxText is Discord's per-field text limit, in characters.
const maxText = 128

// unknownArtist fills Name and State when the event has no artist; the
// event carries no album artist to fall back on.
const unknownArtist = "Unknown Artist"

// Timestamps are unix milliseconds.
type Timestamps struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

type Assets struct {
	LargeImage string `json:"large_image,omitempty"`
	LargeText  string `json:"large_text,omitempty"`
}

// Activity is the Discord wire format.
type Activity struct {
	Type          int         `json:"type"`
	Name          string      `json:"name"`
	Details       string      `json:"details,omitempty"`
	State         string      `json:"state,omitempty"`
	Platform      string      `json:"platform"`
	ApplicationID string      `json:"application_id"`
	Timestamps    *Timestamps `json:"timestamps,omitempty"`
	Assets        *Assets     `json:"assets,omitempty"`
}

func truncate(s string) string {
	r := []rune(s)
	if len(r) <= maxText {
		return s
	}
	return string(r[:maxText])
}

// Build maps a playback event to a "Listening to" activity.
func Build(ev event.Playback, appID string) Activity {
	rate := ev.PlaybackRate
	if rate <= 0 {
		rate = 1
	}
	start := ev.Timestamp*1000 - int64(float64(ev.PositionMs)/rate)
	end := start + int64(float64(ev.Track.DurationMs)/rate)
	artist := truncate(ev.Track.Artist)
	if artist == "" {
		artist = unknownArtist
	}
	return Activity{
		Type:          2,
		Name:          artist,
		Details:       truncate(ev.Track.Title),
		State:         artist,
		Platform:      "desktop",
		ApplicationID: appID,
		Timestamps:    &Timestamps{Start: start, End: end},
		Assets:        &Assets{LargeImage: ev.CoverURL, LargeText: truncate(ev.Track.Album)},
	}
}

// Changed reports whether next differs from prev enough to warrant a Discord update.
// Start/End may drift by up to tolerance without counting as a change.
func Changed(prev, next Activity, tolerance time.Duration) bool {
	if prev.Type != next.Type || prev.Name != next.Name || prev.Details != next.Details ||
		prev.State != next.State || prev.Platform != next.Platform || prev.ApplicationID != next.ApplicationID {
		return true
	}
	var pa, na Assets
	if prev.Assets != nil {
		pa = *prev.Assets
	}
	if next.Assets != nil {
		na = *next.Assets
	}
	if pa != na {
		return true
	}
	if (prev.Timestamps == nil) != (next.Timestamps == nil) {
		return true
	}
	if prev.Timestamps == nil {
		return false
	}
	tol := tolerance.Milliseconds()
	return abs(prev.Timestamps.Start-next.Timestamps.Start) > tol ||
		abs(prev.Timestamps.End-next.Timestamps.End) > tol
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}
