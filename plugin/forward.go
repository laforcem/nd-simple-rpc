package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"

	"github.com/navidrome/navidrome/plugins/pdk/go/host"
	"github.com/navidrome/navidrome/plugins/pdk/go/scrobbler"
)

const coverSize int32 = 512

// Track mirrors the companion's event.Track.
type Track struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	Album      string `json:"album"`
	DurationMs int64  `json:"durationMs"`
}

// Event mirrors the companion's event.Playback; see contract/playback_event.json.
type Event struct {
	Username     string  `json:"username"`
	State        string  `json:"state"`
	PositionMs   int64   `json:"positionMs"`
	PlaybackRate float64 `json:"playbackRate"`
	Timestamp    int64   `json:"timestamp"`
	Track        Track   `json:"track"`
	CoverURL     string  `json:"coverUrl"`
}

// Host is the set of Navidrome host lookups BuildEvent needs.
type Host interface {
	GetSong(username, id string) (title, albumID string, err error)
	TrackArtURL(trackID string, size int32) (string, error)
	AlbumArtURL(albumID string, size int32) (string, error)
}

// BuildEvent converts a playback report into the companion's event. Lookups
// happen only for active states; any lookup failure degrades to the bare
// track title and no cover.
func BuildEvent(req scrobbler.PlaybackReportRequest, h Host) Event {
	ev := Event{
		Username:     req.Username,
		State:        req.State,
		PositionMs:   req.PositionMs,
		PlaybackRate: req.PlaybackRate,
		Timestamp:    req.Timestamp,
		Track: Track{
			ID:         req.Track.ID,
			Title:      req.Track.Title,
			Artist:     req.Track.Artist,
			Album:      req.Track.Album,
			DurationMs: int64(req.Track.Duration * 1000),
		},
	}
	if req.State != "starting" && req.State != "playing" {
		return ev
	}
	title, albumID, err := h.GetSong(req.Username, req.Track.ID)
	if err != nil {
		return ev
	}
	ev.Track.Title = title
	// Navidrome serves the track's own art and falls back to the album's
	// when the track has none, so per-track covers work out of the box.
	if cover, err := h.TrackArtURL(req.Track.ID, coverSize); err == nil {
		ev.CoverURL = cover
		return ev
	}
	if albumID == "" {
		return ev
	}
	if cover, err := h.AlbumArtURL(albumID, coverSize); err == nil {
		ev.CoverURL = cover
	}
	return ev
}

// parseSong extracts title and albumId from a Subsonic getSong JSON response.
func parseSong(body string) (title, albumID string, err error) {
	var resp struct {
		SubsonicResponse struct {
			Status string `json:"status"`
			Song   struct {
				Title   string `json:"title"`
				AlbumID string `json:"albumId"`
			} `json:"song"`
		} `json:"subsonic-response"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return "", "", fmt.Errorf("parse getSong: %w", err)
	}
	r := resp.SubsonicResponse
	if r.Status != "ok" {
		return "", "", fmt.Errorf("getSong status %q", r.Status)
	}
	if r.Song.Title == "" {
		return "", "", errors.New("getSong: missing title")
	}
	return r.Song.Title, r.Song.AlbumID, nil
}

// navidromeHost is the real Host, backed by Navidrome's host functions.
type navidromeHost struct{}

func (navidromeHost) GetSong(username, id string) (string, string, error) {
	uri := "getSong?id=" + url.QueryEscape(id) + "&u=" + url.QueryEscape(username)
	body, err := host.SubsonicAPICall(uri)
	if err != nil {
		return "", "", err
	}
	return parseSong(body)
}

func (navidromeHost) TrackArtURL(trackID string, size int32) (string, error) {
	return host.ArtworkGetTrackUrl(trackID, size)
}

func (navidromeHost) AlbumArtURL(albumID string, size int32) (string, error) {
	return host.ArtworkGetAlbumUrl(albumID, size)
}
