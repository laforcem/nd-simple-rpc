package main

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/navidrome/navidrome/plugins/pdk/go/scrobbler"
)

type fakeHost struct {
	title      string
	albumID    string
	songErr    error
	cover      string
	coverErr   error
	trackCover string
	trackErr   error
	trackCalls int
	gotTrack   string
	songCalls  int
	coverCalls int
	gotUser    string
	gotSongID  string
	gotAlbum   string
	gotSize    int32
}

func (f *fakeHost) GetSong(username, id string) (string, string, error) {
	f.songCalls++
	f.gotUser, f.gotSongID = username, id
	return f.title, f.albumID, f.songErr
}

func (f *fakeHost) TrackArtURL(trackID string, size int32) (string, error) {
	f.trackCalls++
	f.gotTrack, f.gotSize = trackID, size
	return f.trackCover, f.trackErr
}

func (f *fakeHost) AlbumArtURL(albumID string, size int32) (string, error) {
	f.coverCalls++
	f.gotAlbum, f.gotSize = albumID, size
	return f.cover, f.coverErr
}

func request(state string) scrobbler.PlaybackReportRequest {
	return scrobbler.PlaybackReportRequest{
		Username:     "alice",
		State:        state,
		PositionMs:   30000,
		PlaybackRate: 1,
		Timestamp:    1000,
		Track: scrobbler.TrackInfo{
			ID:       "tr-123",
			Title:    "Waterfall",
			Artist:   "Some Artist feat. Guest",
			Album:    "Cascade",
			Duration: 180,
		},
	}
}

func fullHost() *fakeHost {
	return &fakeHost{
		title:      "Waterfall (instrumental mix)",
		albumID:    "al-1",
		cover:      "https://navidrome.example/rest/getCoverArt?id=al-1&size=512&t=tok",
		trackCover: "https://navidrome.example/rest/getCoverArt?id=mf-tr-123&size=512&t=tok",
	}
}

func TestBuildEventPlaying(t *testing.T) {
	h := fullHost()
	ev := BuildEvent(request("playing"), h)
	if ev.Track.Title != "Waterfall (instrumental mix)" {
		t.Errorf("title = %q", ev.Track.Title)
	}
	if ev.CoverURL != h.trackCover {
		t.Errorf("coverUrl = %q", ev.CoverURL)
	}
	if h.coverCalls != 0 {
		t.Errorf("album art looked up although track art succeeded")
	}
	if h.gotUser != "alice" || h.gotSongID != "tr-123" || h.gotTrack != "tr-123" || h.gotSize != 512 {
		t.Errorf("lookup args: %+v", h)
	}
	if ev.Track.DurationMs != 180000 || ev.Timestamp != 1000 || ev.PositionMs != 30000 {
		t.Errorf("passthrough fields: %+v", ev)
	}
}

func TestBuildEventStartingLooksUp(t *testing.T) {
	h := fullHost()
	ev := BuildEvent(request("starting"), h)
	if h.songCalls != 1 || h.trackCalls != 1 {
		t.Fatalf("calls: song=%d track=%d", h.songCalls, h.trackCalls)
	}
	if ev.State != "starting" || ev.Track.Title != "Waterfall (instrumental mix)" || ev.CoverURL == "" {
		t.Errorf("event: %+v", ev)
	}
}

func TestBuildEventPausedNoLookups(t *testing.T) {
	for _, state := range []string{"paused", "stopped", "expired"} {
		h := fullHost()
		ev := BuildEvent(request(state), h)
		if h.songCalls != 0 || h.trackCalls != 0 || h.coverCalls != 0 {
			t.Errorf("%s: lookups made: song=%d track=%d album=%d", state, h.songCalls, h.trackCalls, h.coverCalls)
		}
		if ev.Track.Title != "Waterfall" || ev.CoverURL != "" {
			t.Errorf("%s: event: %+v", state, ev)
		}
	}
}

func TestBuildEventGetSongFails(t *testing.T) {
	h := fullHost()
	h.songErr = errors.New("boom")
	ev := BuildEvent(request("playing"), h)
	if ev.Track.Title != "Waterfall" {
		t.Errorf("title = %q, want bare title", ev.Track.Title)
	}
	if ev.CoverURL != "" {
		t.Errorf("coverUrl = %q, want empty", ev.CoverURL)
	}
	if h.trackCalls != 0 || h.coverCalls != 0 {
		t.Errorf("artwork looked up despite getSong failure")
	}
}

func TestBuildEventTrackArtFailsFallsBackToAlbum(t *testing.T) {
	h := fullHost()
	h.trackErr = errors.New("boom")
	ev := BuildEvent(request("playing"), h)
	if ev.CoverURL != h.cover || h.gotAlbum != "al-1" {
		t.Errorf("coverUrl = %q, album = %q", ev.CoverURL, h.gotAlbum)
	}
}

func TestBuildEventArtworkFails(t *testing.T) {
	h := fullHost()
	h.trackErr = errors.New("boom")
	h.coverErr = errors.New("boom")
	ev := BuildEvent(request("playing"), h)
	if ev.Track.Title != "Waterfall (instrumental mix)" {
		t.Errorf("title = %q", ev.Track.Title)
	}
	if ev.CoverURL != "" {
		t.Errorf("coverUrl = %q, want empty", ev.CoverURL)
	}
}

func TestParseSong(t *testing.T) {
	ok := `{"subsonic-response":{"status":"ok","song":{"title":"T","albumId":"al-9"}}}`
	title, album, err := parseSong(ok)
	if err != nil || title != "T" || album != "al-9" {
		t.Errorf("ok: %q %q %v", title, album, err)
	}
	for name, body := range map[string]string{
		"failed":       `{"subsonic-response":{"status":"failed","error":{"code":70}}}`,
		"missingTitle": `{"subsonic-response":{"status":"ok","song":{"albumId":"a"}}}`,
		"noSong":       `{"subsonic-response":{"status":"ok"}}`,
		"badJSON":      `nope`,
	} {
		if _, _, err := parseSong(body); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestEventJSONMatchesContract(t *testing.T) {
	raw, err := os.ReadFile("../contract/playback_event.json")
	if err != nil {
		t.Fatal(err)
	}
	var want any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}

	h := fullHost()
	req := request("playing")
	ev := BuildEvent(req, h)
	got, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	var gotAny any
	if err := json.Unmarshal(got, &gotAny); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, gotAny) {
		t.Errorf("JSON mismatch\nwant %s\ngot  %s", raw, got)
	}
}
