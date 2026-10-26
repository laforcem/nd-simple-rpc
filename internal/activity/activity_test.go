package activity

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/laforcem/nd-simple-rpc/internal/event"
)

func fixture(t *testing.T) event.Playback {
	t.Helper()
	b, err := os.ReadFile("../../contract/playback_event.json")
	if err != nil {
		t.Fatal(err)
	}
	var p event.Playback
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBuildMapsCard(t *testing.T) {
	ev := fixture(t)
	a := Build(ev, "app1")
	if a.Type != 2 || a.Platform != "desktop" || a.ApplicationID != "app1" {
		t.Errorf("bad constants: %+v", a)
	}
	if a.Name != ev.Track.Artist || a.State != ev.Track.Artist {
		t.Errorf("name/state = %q/%q", a.Name, a.State)
	}
	if a.Details != "Waterfall (instrumental mix)" {
		t.Errorf("details = %q", a.Details)
	}
	if a.Assets == nil || a.Assets.LargeText != ev.Track.Album || a.Assets.LargeImage != ev.CoverURL {
		t.Errorf("assets = %+v", a.Assets)
	}
}

func TestBuildTimestamps(t *testing.T) {
	ev := event.Playback{Timestamp: 1000, PositionMs: 30000, PlaybackRate: 1, Track: event.Track{DurationMs: 180000}}
	a := Build(ev, "x")
	if a.Timestamps.Start != 970000 || a.Timestamps.End != 1150000 {
		t.Errorf("rate 1: %+v", a.Timestamps)
	}
	ev.PlaybackRate = 2
	a = Build(ev, "x")
	if a.Timestamps.Start != 1000000-15000 || a.Timestamps.End != a.Timestamps.Start+90000 {
		t.Errorf("rate 2: %+v", a.Timestamps)
	}
	ev.PlaybackRate = 0
	a = Build(ev, "x")
	if a.Timestamps.Start != 970000 || a.Timestamps.End != 1150000 {
		t.Errorf("rate 0: %+v", a.Timestamps)
	}
}

func TestBuildNoCover(t *testing.T) {
	ev := fixture(t)
	ev.CoverURL = ""
	a := Build(ev, "x")
	if a.Assets.LargeImage != "" {
		t.Errorf("LargeImage = %q", a.Assets.LargeImage)
	}
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "large_image") {
		t.Errorf("json has large_image: %s", b)
	}
}

func TestBuildTruncatesRunes(t *testing.T) {
	ev := fixture(t)
	ev.Track.Title = strings.Repeat("滝", 200)
	a := Build(ev, "x")
	if n := utf8.RuneCountInString(a.Details); n != 128 {
		t.Errorf("rune count = %d", n)
	}
	if !utf8.ValidString(a.Details) {
		t.Error("invalid utf8")
	}
}

func TestChanged(t *testing.T) {
	base := Build(fixture(t), "x")
	tol := 3 * time.Second
	clone := func() Activity {
		c := base
		ts := *base.Timestamps
		c.Timestamps = &ts
		as := *base.Assets
		c.Assets = &as
		return c
	}

	d := clone()
	d.Details = "other"
	if !Changed(base, d, tol) {
		t.Error("different details should be changed")
	}

	s := clone()
	s.Timestamps.Start += 2000
	s.Timestamps.End += 2000
	if Changed(base, s, tol) {
		t.Error("2s shift within tolerance should be unchanged")
	}
	s.Timestamps.Start += 3000
	s.Timestamps.End += 3000
	if !Changed(base, s, tol) {
		t.Error("5s shift should be changed")
	}

	i := clone()
	i.Assets.LargeImage = "https://other/img"
	if !Changed(base, i, tol) {
		t.Error("different large image should be changed")
	}
}

func TestBuildEmptyArtist(t *testing.T) {
	ev := fixture(t)
	ev.Track.Artist = ""
	a := Build(ev, "app1")
	if a.Name != "Unknown Artist" || a.State != "Unknown Artist" {
		t.Errorf("name/state = %q/%q, want Unknown Artist", a.Name, a.State)
	}
}
