package event

import (
	"encoding/json"
	"os"
	"testing"
)

func TestPlaybackDecodesContractFixture(t *testing.T) {
	b, err := os.ReadFile("../../contract/playback_event.json")
	if err != nil {
		t.Fatal(err)
	}
	var p Playback
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	want := Playback{
		Username: "alice", State: "playing", PositionMs: 30000, PlaybackRate: 1, Timestamp: 1000,
		Track:    Track{ID: "tr-123", Title: "Waterfall (instrumental mix)", Artist: "Some Artist feat. Guest", Album: "Cascade", DurationMs: 180000},
		CoverURL: "https://navidrome.example/rest/getCoverArt?id=al-1&size=512&t=tok",
	}
	if p != want {
		t.Fatalf("got %+v, want %+v", p, want)
	}
}

func TestActive(t *testing.T) {
	for s, want := range map[string]bool{"starting": true, "playing": true, "paused": false, "stopped": false, "expired": false, "": false} {
		if got := Active(s); got != want {
			t.Errorf("Active(%q) = %v, want %v", s, got, want)
		}
	}
}
