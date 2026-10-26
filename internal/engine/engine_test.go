package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/laforcem/nd-simple-rpc/internal/activity"
	"github.com/laforcem/nd-simple-rpc/internal/discord"
	"github.com/laforcem/nd-simple-rpc/internal/event"
	"github.com/laforcem/nd-simple-rpc/internal/store"
	"github.com/laforcem/nd-simple-rpc/internal/tokens"
)

// --- fakes ---

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type call struct {
	at      time.Time
	op      string // "put" or "delete"
	access  string
	session string
	act     activity.Activity
}

type fakePub struct {
	mu      sync.Mutex
	clk     *clock
	calls   []call
	putErrs []error
	delErrs []error
	n       int
}

func (p *fakePub) PutSession(_ context.Context, access, session string, a activity.Activity) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, call{at: p.clk.now(), op: "put", access: access, session: session, act: a})
	if len(p.putErrs) > 0 {
		err := p.putErrs[0]
		p.putErrs = p.putErrs[1:]
		if err != nil {
			return "", err
		}
	}
	if session != "" {
		return session, nil
	}
	p.n++
	return fmt.Sprintf("sess-%d", p.n), nil
}

func (p *fakePub) DeleteSession(_ context.Context, access, session string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, call{at: p.clk.now(), op: "delete", access: access, session: session})
	if len(p.delErrs) > 0 {
		err := p.delErrs[0]
		p.delErrs = p.delErrs[1:]
		return err
	}
	return nil
}

func (p *fakePub) all() []call {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]call(nil), p.calls...)
}

func (p *fakePub) last(t *testing.T) call {
	t.Helper()
	c := p.all()
	if len(c) == 0 {
		t.Fatal("no publisher calls")
	}
	return c[len(c)-1]
}

type fakeUsers struct {
	mu sync.Mutex
	m  map[string]store.User
}

func (u *fakeUsers) Get(_ context.Context, name string) (store.User, bool, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	x, ok := u.m[name]
	return x, ok, nil
}

func (u *fakeUsers) SetSessionToken(_ context.Context, name, tok string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	x, ok := u.m[name]
	if ok {
		x.SessionToken = tok
		u.m[name] = x
	}
	return nil
}

func (u *fakeUsers) ListWithSessions(context.Context) ([]store.User, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	var out []store.User
	for _, x := range u.m {
		if x.SessionToken != "" {
			out = append(out, x)
		}
	}
	return out, nil
}

func (u *fakeUsers) session(name string) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.m[name].SessionToken
}

func (u *fakeUsers) update(name string, f func(*store.User)) {
	u.mu.Lock()
	defer u.mu.Unlock()
	x := u.m[name]
	f(&x)
	u.m[name] = x
}

type fakeTokens struct {
	mu           sync.Mutex
	notConnected map[string]bool
	calls        int
}

func (f *fakeTokens) AccessToken(_ context.Context, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.notConnected[name] {
		return "", tokens.ErrNotConnected
	}
	return "access-" + name, nil
}

func (f *fakeTokens) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type harness struct {
	e   *Engine
	clk *clock
	pub *fakePub
	us  *fakeUsers
	tok *fakeTokens
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	clk := &clock{t: time.Unix(1_000_000, 0)}
	h := &harness{
		clk: clk,
		pub: &fakePub{clk: clk},
		us:  &fakeUsers{m: map[string]store.User{"alice": {Username: "alice"}}},
		tok: &fakeTokens{notConnected: map[string]bool{}},
	}
	cfg := Config{AppID: "app", Keepalive: 15 * time.Minute, Grace: 30 * time.Second,
		Tolerance: 3 * time.Second, RateMax: 4, RateWindow: 20 * time.Second}
	h.e = New(cfg, h.us, h.tok, h.pub, clk.now)
	return h
}

// ev builds an event for alice stamped at the fake clock's current second.
func (h *harness) ev(state, title string, posMs, durMs int64) event.Playback {
	return event.Playback{
		Username: "alice", State: state, PositionMs: posMs, PlaybackRate: 1,
		Timestamp: h.clk.now().Unix(),
		Track:     event.Track{ID: title, Title: title, Artist: "Artist", Album: "Album", DurationMs: durMs},
	}
}

func (h *harness) play(title string, posMs int64) {
	h.e.HandlePlayback(h.ev("playing", title, posMs, 180_000))
}

func (h *harness) tick() { h.e.Tick(context.Background()) }

func wantCalls(t *testing.T, p *fakePub, n int) []call {
	t.Helper()
	c := p.all()
	if len(c) != n {
		t.Fatalf("got %d publisher calls, want %d: %+v", len(c), n, c)
	}
	return c
}

// --- tests ---

func TestPlayCreates(t *testing.T) {
	h := newHarness(t)
	h.play("A", 0)
	h.tick()
	c := wantCalls(t, h.pub, 1)[0]
	if c.op != "put" || c.session != "" || c.act.Details != "A" || c.access != "access-alice" {
		t.Fatalf("unexpected call %+v", c)
	}
	if got := h.us.session("alice"); got != "sess-1" {
		t.Fatalf("stored session = %q, want sess-1", got)
	}
	h.tick()
	wantCalls(t, h.pub, 1)
}

func TestPauseDeletes(t *testing.T) {
	h := newHarness(t)
	h.play("A", 0)
	h.tick()
	h.clk.advance(time.Second)
	h.e.HandlePlayback(h.ev("paused", "A", 1000, 180_000))
	h.tick()
	c := wantCalls(t, h.pub, 2)[1]
	if c.op != "delete" || c.session != "sess-1" {
		t.Fatalf("unexpected call %+v", c)
	}
	if got := h.us.session("alice"); got != "" {
		t.Fatalf("stored session = %q, want empty", got)
	}
	h.tick()
	wantCalls(t, h.pub, 2)
}

func TestResumeRecreates(t *testing.T) {
	h := newHarness(t)
	h.play("A", 0)
	h.tick()
	h.clk.advance(time.Second)
	h.e.HandlePlayback(h.ev("paused", "A", 1000, 180_000))
	h.tick()
	h.clk.advance(5 * time.Second)
	h.play("A", 1000)
	h.tick()
	c := wantCalls(t, h.pub, 3)[2]
	if c.op != "put" || c.session != "" || c.act.Details != "A" {
		t.Fatalf("unexpected call %+v", c)
	}
	if got := h.us.session("alice"); got != "sess-2" {
		t.Fatalf("stored session = %q, want sess-2", got)
	}
}

func TestStoppedAndExpiredDelete(t *testing.T) {
	for _, state := range []string{"stopped", "expired"} {
		t.Run(state, func(t *testing.T) {
			h := newHarness(t)
			h.play("A", 0)
			h.tick()
			h.clk.advance(time.Second)
			h.e.HandlePlayback(h.ev(state, "A", 1000, 180_000))
			h.tick()
			if c := wantCalls(t, h.pub, 2)[1]; c.op != "delete" || c.session != "sess-1" {
				t.Fatalf("unexpected call %+v", c)
			}
		})
	}
}

func TestStartingTreatedAsPlaying(t *testing.T) {
	h := newHarness(t)
	h.e.HandlePlayback(h.ev("starting", "A", 0, 180_000))
	h.tick()
	if c := wantCalls(t, h.pub, 1)[0]; c.op != "put" || c.act.Details != "A" {
		t.Fatalf("unexpected call %+v", c)
	}
}

func TestSameTrackNoRepost(t *testing.T) {
	h := newHarness(t)
	h.play("A", 30_000)
	h.tick()
	h.clk.advance(10 * time.Second)
	h.play("A", 40_000) // same track, consistent progress: same timestamps
	h.tick()
	h.clk.advance(10 * time.Second)
	h.play("A", 51_000) // 1 s drift: within the 3 s tolerance
	h.tick()
	wantCalls(t, h.pub, 1)
}

func TestSeekBeyondToleranceUpdates(t *testing.T) {
	h := newHarness(t)
	h.play("A", 30_000)
	h.tick()
	h.clk.advance(time.Second)
	h.play("A", 90_000)
	h.tick()
	c := wantCalls(t, h.pub, 2)[1]
	if c.op != "put" || c.session != "sess-1" {
		t.Fatalf("want update on sess-1, got %+v", c)
	}
	want := activity.Build(h.ev("playing", "A", 90_000, 180_000), "app")
	if activity.Changed(c.act, want, 0) {
		t.Fatalf("posted %+v, want %+v", c.act, want)
	}
}

func TestKeepaliveAfter15m(t *testing.T) {
	h := newHarness(t)
	h.e.HandlePlayback(h.ev("playing", "Long", 0, int64(time.Hour/time.Millisecond)))
	h.tick()
	h.clk.advance(15*time.Minute - time.Second)
	h.tick()
	wantCalls(t, h.pub, 1)
	h.clk.advance(time.Second)
	h.tick()
	c := wantCalls(t, h.pub, 2)
	if c[1].op != "put" || c[1].session != "sess-1" || activity.Changed(c[0].act, c[1].act, 0) {
		t.Fatalf("want identical re-post on sess-1, got %+v", c[1])
	}
	h.tick()
	wantCalls(t, h.pub, 2)
}

func TestStaleClears(t *testing.T) {
	h := newHarness(t)
	h.play("A", 30_000) // 150 s left + 30 s grace = stale after 180 s
	h.tick()
	h.clk.advance(180 * time.Second)
	h.tick()
	wantCalls(t, h.pub, 1)
	h.clk.advance(time.Second)
	h.tick()
	if c := wantCalls(t, h.pub, 2)[1]; c.op != "delete" || c.session != "sess-1" {
		t.Fatalf("unexpected call %+v", c)
	}
	h.tick()
	wantCalls(t, h.pub, 2)
}

func TestSkipSpamCoalesces(t *testing.T) {
	h := newHarness(t)
	start := h.clk.now()
	// Five tracks at t=0, 0.5, 1, 1.5, 2.5 s; tick every 0.5 s for 40 s.
	events := map[int]string{0: "Track 1", 1: "Track 2", 2: "Track 3", 3: "Track 4", 5: "Track 5"}
	for step := 0; step <= 80; step++ {
		if title, ok := events[step]; ok {
			h.play(title, 0)
		}
		h.tick()
		h.clk.advance(500 * time.Millisecond)
	}
	calls := h.pub.all()
	for i, c := range calls {
		n := 0
		for _, d := range calls[i:] {
			if d.at.Sub(c.at) <= 20*time.Second {
				n++
			}
		}
		if n > 4 {
			t.Fatalf("%d calls within 20 s of call %d at +%v", n, i, c.at.Sub(start))
		}
	}
	if len(calls) == 0 || calls[len(calls)-1].act.Details != "Track 5" {
		t.Fatalf("last posted call is not Track 5: %+v", calls)
	}
	seen5 := false
	for _, c := range calls {
		if c.act.Details == "Track 5" {
			seen5 = true
		} else if seen5 {
			t.Fatalf("stale track %q posted after Track 5", c.act.Details)
		}
	}
	for _, c := range calls {
		if c.op != "put" {
			t.Fatalf("unexpected %s call", c.op)
		}
	}
}

func Test429BacksOff(t *testing.T) {
	h := newHarness(t)
	h.pub.putErrs = []error{&discord.RateLimitError{RetryAfter: 3 * time.Second}}
	h.play("A", 0)
	h.tick()
	wantCalls(t, h.pub, 1)
	for _, d := range []time.Duration{time.Second, time.Second, 900 * time.Millisecond} {
		h.clk.advance(d)
		h.tick()
	}
	wantCalls(t, h.pub, 1)
	h.clk.advance(100 * time.Millisecond)
	h.tick()
	c := wantCalls(t, h.pub, 2)[1]
	if c.op != "put" || c.act.Details != "A" {
		t.Fatalf("unexpected retry %+v", c)
	}
	if got := h.us.session("alice"); got != "sess-1" {
		t.Fatalf("stored session = %q, want sess-1", got)
	}
}

func TestSessionGoneRecreates(t *testing.T) {
	h := newHarness(t)
	h.play("A", 0)
	h.tick()
	h.clk.advance(time.Second)
	h.play("B", 0)
	h.pub.putErrs = []error{discord.ErrSessionGone}
	h.tick()
	if got := h.us.session("alice"); got != "" {
		t.Fatalf("stored session = %q, want cleared", got)
	}
	h.clk.advance(time.Second)
	h.tick()
	c := wantCalls(t, h.pub, 3)
	if c[1].session != "sess-1" || c[2].op != "put" || c[2].session != "" || c[2].act.Details != "B" {
		t.Fatalf("want failed update then create, got %+v", c)
	}
	if got := h.us.session("alice"); got != "sess-2" {
		t.Fatalf("stored session = %q, want sess-2", got)
	}
}

func TestSessionGoneOnDeleteClears(t *testing.T) {
	h := newHarness(t)
	h.play("A", 0)
	h.tick()
	h.e.HandlePlayback(h.ev("stopped", "A", 0, 180_000))
	h.pub.delErrs = []error{discord.ErrSessionGone}
	h.tick()
	h.tick()
	wantCalls(t, h.pub, 2)
	if got := h.us.session("alice"); got != "" {
		t.Fatalf("stored session = %q, want cleared", got)
	}
}

func TestUserPausedSuppresses(t *testing.T) {
	h := newHarness(t)
	h.us.update("alice", func(u *store.User) { u.Paused = true })
	h.play("A", 0)
	h.tick()
	h.tick()
	wantCalls(t, h.pub, 0)
}

func TestNotConnectedIgnored(t *testing.T) {
	t.Run("no store row", func(t *testing.T) {
		h := newHarness(t)
		h.e.HandlePlayback(event.Playback{Username: "mallory", State: "playing", Timestamp: h.clk.now().Unix(),
			Track: event.Track{Title: "X", DurationMs: 180_000}})
		h.tick()
		h.tick()
		wantCalls(t, h.pub, 0)
	})
	t.Run("needs reconnect", func(t *testing.T) {
		h := newHarness(t)
		h.us.update("alice", func(u *store.User) { u.NeedsReconnect = true })
		h.play("A", 0)
		h.tick()
		wantCalls(t, h.pub, 0)
	})
	t.Run("token manager says not connected", func(t *testing.T) {
		h := newHarness(t)
		h.tok.notConnected["alice"] = true
		h.play("A", 0)
		h.tick()
		h.tick()
		h.tick()
		wantCalls(t, h.pub, 0)
		if n := h.tok.count(); n != 1 {
			t.Fatalf("AccessToken called %d times, want 1 (state dropped after ErrNotConnected)", n)
		}
	})
}

func TestUnauthorizedRetriesNextTick(t *testing.T) {
	h := newHarness(t)
	h.pub.putErrs = []error{discord.ErrUnauthorized}
	h.play("A", 0)
	h.tick()
	h.clk.advance(time.Second)
	h.tick()
	if c := wantCalls(t, h.pub, 2)[1]; c.op != "put" || c.session != "" {
		t.Fatalf("unexpected retry %+v", c)
	}
}

func TestClearDeletesSynchronously(t *testing.T) {
	ctx := context.Background()
	t.Run("deletes now", func(t *testing.T) {
		h := newHarness(t)
		h.play("A", 0)
		h.tick()
		if err := h.e.Clear(ctx, "alice"); err != nil {
			t.Fatal(err)
		}
		if c := wantCalls(t, h.pub, 2)[1]; c.op != "delete" || c.session != "sess-1" {
			t.Fatalf("unexpected call %+v", c)
		}
		if got := h.us.session("alice"); got != "" {
			t.Fatalf("stored session = %q, want cleared", got)
		}
		h.tick()
		wantCalls(t, h.pub, 2)
	})
	t.Run("ignores rolling window", func(t *testing.T) {
		h := newHarness(t)
		for i := 0; i < 4; i++ {
			h.play(fmt.Sprintf("T%d", i), 0)
			h.tick()
		}
		wantCalls(t, h.pub, 4)
		if err := h.e.Clear(ctx, "alice"); err != nil {
			t.Fatal(err)
		}
		if c := wantCalls(t, h.pub, 5)[4]; c.op != "delete" {
			t.Fatalf("unexpected call %+v", c)
		}
	})
	t.Run("no session is a no-op", func(t *testing.T) {
		h := newHarness(t)
		if err := h.e.Clear(ctx, "alice"); err != nil {
			t.Fatal(err)
		}
		wantCalls(t, h.pub, 0)
	})
	t.Run("429 returns error and tick retries", func(t *testing.T) {
		h := newHarness(t)
		h.play("A", 0)
		h.tick()
		h.pub.delErrs = []error{&discord.RateLimitError{RetryAfter: 2 * time.Second}}
		var rl *discord.RateLimitError
		if err := h.e.Clear(ctx, "alice"); !errors.As(err, &rl) {
			t.Fatalf("Clear err = %v, want RateLimitError", err)
		}
		// A second Clear during the back-off must not call Discord.
		if err := h.e.Clear(ctx, "alice"); !errors.As(err, &rl) {
			t.Fatalf("Clear during back-off err = %v, want RateLimitError", err)
		}
		h.tick()
		wantCalls(t, h.pub, 2)
		h.clk.advance(2 * time.Second)
		h.tick()
		if c := wantCalls(t, h.pub, 3)[2]; c.op != "delete" || c.session != "sess-1" {
			t.Fatalf("unexpected retry %+v", c)
		}
		if got := h.us.session("alice"); got != "" {
			t.Fatalf("stored session = %q, want cleared", got)
		}
	})
}

func TestStartupDeletesStoredSessions(t *testing.T) {
	h := newHarness(t)
	h.us.m["alice"] = store.User{Username: "alice", SessionToken: "old-a"}
	h.us.m["bob"] = store.User{Username: "bob", SessionToken: "old-b"}
	h.us.m["carol"] = store.User{Username: "carol"}
	if err := h.e.Startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, c := range wantCalls(t, h.pub, 2) {
		if c.op != "delete" {
			t.Fatalf("unexpected call %+v", c)
		}
		got[c.session] = c.access
	}
	if got["old-a"] != "access-alice" || got["old-b"] != "access-bob" {
		t.Fatalf("deleted %v", got)
	}
	for _, u := range []string{"alice", "bob"} {
		if s := h.us.session(u); s != "" {
			t.Fatalf("%s stored session = %q, want cleared", u, s)
		}
	}
	h.tick()
	wantCalls(t, h.pub, 2)
}

func TestStartupFailureRetriedByTick(t *testing.T) {
	h := newHarness(t)
	h.us.m["alice"] = store.User{Username: "alice", SessionToken: "old-a"}
	h.pub.delErrs = []error{errors.New("boom")}
	if err := h.e.Startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.tick()
	if c := wantCalls(t, h.pub, 2)[1]; c.op != "delete" || c.session != "old-a" {
		t.Fatalf("unexpected retry %+v", c)
	}
	if s := h.us.session("alice"); s != "" {
		t.Fatalf("stored session = %q, want cleared", s)
	}
}

func TestHandlePlaybackNeverCallsPublisher(t *testing.T) {
	h := newHarness(t)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h.play(fmt.Sprintf("T%d", i), 0)
		}(i)
	}
	wg.Wait()
	wantCalls(t, h.pub, 0)
	if n := h.tok.count(); n != 0 {
		t.Fatalf("AccessToken called %d times", n)
	}
}

func TestConcurrentHandleTickClear(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(3)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				h.play(fmt.Sprintf("T%d-%d", i, j), 0)
			}
		}(i)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				h.tick()
				h.clk.advance(100 * time.Millisecond)
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				_ = h.e.Clear(ctx, "alice")
			}
		}()
	}
	wg.Wait()
}

func TestRunTicksUntilCancelled(t *testing.T) {
	h := newHarness(t)
	h.play("A", 0)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.e.Run(ctx); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for len(h.pub.all()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	wantCalls(t, h.pub, 1)
}

// The production types satisfy the engine's interfaces.
var (
	_ Users     = (*store.Store)(nil)
	_ Tokens    = (*tokens.Manager)(nil)
	_ Publisher = (*discord.Client)(nil)
)
