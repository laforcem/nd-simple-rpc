// Package event defines the playback event the Navidrome plugin forwards to the companion.
package event

// Track is the track being played.
type Track struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	Album      string `json:"album"`
	DurationMs int64  `json:"durationMs"`
}

// Playback is one playback state report. Timestamp is unix seconds, as Navidrome sends it.
type Playback struct {
	Username     string  `json:"username"`
	State        string  `json:"state"`
	PositionMs   int64   `json:"positionMs"`
	PlaybackRate float64 `json:"playbackRate"`
	Timestamp    int64   `json:"timestamp"`
	Track        Track   `json:"track"`
	CoverURL     string  `json:"coverUrl"`
}

// Active reports whether the state means audio is playing.
func Active(state string) bool {
	return state == "starting" || state == "playing"
}
