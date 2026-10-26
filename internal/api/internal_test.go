package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/laforcem/nd-simple-rpc/internal/event"
)

type fakeEngine struct {
	mu  sync.Mutex
	got []event.Playback
}

func (f *fakeEngine) HandlePlayback(p event.Playback) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, p)
}

func post(h http.Handler, method, path, auth, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func fixture(t *testing.T) string {
	b, err := os.ReadFile("../../contract/playback_event.json")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestInternalRejectsBadSecret(t *testing.T) {
	f := &fakeEngine{}
	h := NewInternal("s3cret", f)
	for _, auth := range []string{"", "Bearer wrong", "s3cret", "Basic s3cret"} {
		if w := post(h, "POST", "/internal/v1/playback", auth, fixture(t)); w.Code != 401 {
			t.Errorf("auth %q: code %d, want 401", auth, w.Code)
		}
	}
	if len(f.got) != 0 {
		t.Fatal("engine got an event despite bad secret")
	}
}

func TestInternalAcceptsContractFixture(t *testing.T) {
	f := &fakeEngine{}
	h := NewInternal("s3cret", f)
	w := post(h, "POST", "/internal/v1/playback", "Bearer s3cret", fixture(t))
	if w.Code != 202 {
		t.Fatalf("code %d", w.Code)
	}
	if len(f.got) != 1 || f.got[0].Username != "alice" || f.got[0].Timestamp != 1000 || f.got[0].Track.ID != "tr-123" {
		t.Fatalf("got %+v", f.got)
	}
}

func TestInternalZeroTimestampGetsNow(t *testing.T) {
	f := &fakeEngine{}
	h := NewInternal("s", f)
	before := time.Now().Unix()
	w := post(h, "POST", "/internal/v1/playback", "Bearer s", `{"username":"bob","state":"playing"}`)
	if w.Code != 202 {
		t.Fatalf("code %d", w.Code)
	}
	if ts := f.got[0].Timestamp; ts < before || ts > time.Now().Unix() {
		t.Fatalf("timestamp %d not now", ts)
	}
}

func TestInternalBadBody(t *testing.T) {
	f := &fakeEngine{}
	h := NewInternal("s", f)
	big := `{"username":"a","pad":"` + strings.Repeat("x", 70<<10) + `"}`
	for name, body := range map[string]string{"garbage": "nope", "empty": "", "nouser": `{"state":"playing"}`, "toobig": big} {
		if w := post(h, "POST", "/internal/v1/playback", "Bearer s", body); w.Code != 400 {
			t.Errorf("%s: code %d, want 400", name, w.Code)
		}
	}
	if len(f.got) != 0 {
		t.Fatal("engine got an event")
	}
}

func TestInternalMethodAndPath(t *testing.T) {
	h := NewInternal("s", &fakeEngine{})
	if w := post(h, "GET", "/internal/v1/playback", "Bearer s", ""); w.Code != 405 {
		t.Errorf("GET: %d, want 405", w.Code)
	}
	if w := post(h, "POST", "/nope", "Bearer s", "{}"); w.Code != 404 {
		t.Errorf("unknown path: %d, want 404", w.Code)
	}
}

func TestHealthz(t *testing.T) {
	h := NewInternal("s", &fakeEngine{})
	if w := post(h, "GET", "/healthz", "", ""); w.Code != 200 {
		t.Fatalf("code %d", w.Code)
	}
}
