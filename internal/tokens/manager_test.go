package tokens

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/laforcem/nd-simple-rpc/internal/discord"
	"github.com/laforcem/nd-simple-rpc/internal/store"
)

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

type fakeRefresher struct {
	mu    sync.Mutex
	calls int
	got   []string
	resp  discord.TokenSet
	err   error
	delay time.Duration
	// during runs inside Refresh, between the manager's read and its persist.
	during func()
}

func (f *fakeRefresher) Refresh(ctx context.Context, rt string) (discord.TokenSet, error) {
	f.mu.Lock()
	f.calls++
	f.got = append(f.got, rt)
	f.mu.Unlock()
	time.Sleep(f.delay)
	if f.during != nil {
		f.during()
	}
	return f.resp, f.err
}

func (f *fakeRefresher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func key() []byte { return bytes.Repeat([]byte{7}, 32) }

func setup(t *testing.T, u *store.User, f *fakeRefresher) (*Manager, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t.db")
	s, err := store.Open(path, key())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if u != nil {
		if err := s.Put(context.Background(), *u); err != nil {
			t.Fatal(err)
		}
	}
	return &Manager{Store: s, Discord: f, Now: func() time.Time { return t0 }}, path
}

func user(expiresIn time.Duration) *store.User {
	return &store.User{Username: "Malc", RefreshToken: "rt-old", AccessToken: "at-old",
		AccessExpiresAt: t0.Add(expiresIn), Paused: true, SessionToken: "sess"}
}

func TestFreshTokenNoRefresh(t *testing.T) {
	f := &fakeRefresher{}
	m, _ := setup(t, user(time.Hour), f)
	got, err := m.AccessToken(context.Background(), "Malc")
	if err != nil || got != "at-old" {
		t.Fatalf("got %q, %v", got, err)
	}
	if f.count() != 0 {
		t.Errorf("Refresh called %d times", f.count())
	}
}

func TestNearExpiryRefreshesAndPersists(t *testing.T) {
	exp := t0.Add(2 * time.Hour)
	f := &fakeRefresher{resp: discord.TokenSet{AccessToken: "at-new", RefreshToken: "rt-new", ExpiresAt: exp}}
	m, _ := setup(t, user(4*time.Minute), f)
	got, err := m.AccessToken(context.Background(), "Malc")
	if err != nil || got != "at-new" {
		t.Fatalf("got %q, %v", got, err)
	}
	if len(f.got) != 1 || f.got[0] != "rt-old" {
		t.Errorf("refresh used %v", f.got)
	}
	u, ok, err := m.Store.Get(context.Background(), "Malc")
	if err != nil || !ok {
		t.Fatal(err, ok)
	}
	if u.AccessToken != "at-new" || u.RefreshToken != "rt-new" || !u.AccessExpiresAt.Equal(exp) {
		t.Errorf("persisted %+v", u)
	}
	if !u.Paused || u.SessionToken != "sess" {
		t.Errorf("other fields clobbered: %+v", u)
	}
}

func TestEmptyRefreshTokenKeepsOld(t *testing.T) {
	f := &fakeRefresher{resp: discord.TokenSet{AccessToken: "at-new", ExpiresAt: t0.Add(time.Hour)}}
	m, _ := setup(t, user(0), f)
	if _, err := m.AccessToken(context.Background(), "Malc"); err != nil {
		t.Fatal(err)
	}
	u, _, _ := m.Store.Get(context.Background(), "Malc")
	if u.RefreshToken != "rt-old" || u.AccessToken != "at-new" {
		t.Errorf("persisted %+v", u)
	}
}

func TestRefreshPersistsBeforeReturn(t *testing.T) {
	f := &fakeRefresher{resp: discord.TokenSet{AccessToken: "at-new", RefreshToken: "rt-new", ExpiresAt: t0.Add(time.Hour)}}
	m, path := setup(t, user(time.Minute), f)
	if _, err := m.AccessToken(context.Background(), "Malc"); err != nil {
		t.Fatal(err)
	}
	if f.count() != 1 {
		t.Fatalf("calls = %d", f.count())
	}
	// Simulate a crash: reopen from disk.
	if err := m.Store.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := store.Open(path, key())
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	u, ok, err := s2.Get(context.Background(), "Malc")
	if err != nil || !ok || u.RefreshToken != "rt-new" {
		t.Fatalf("after reopen: %+v %v %v", u, ok, err)
	}
}

func TestConcurrentCallsRefreshOnce(t *testing.T) {
	f := &fakeRefresher{delay: 50 * time.Millisecond,
		resp: discord.TokenSet{AccessToken: "at-new", RefreshToken: "rt-new", ExpiresAt: t0.Add(time.Hour)}}
	m, _ := setup(t, user(time.Minute), f)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := m.AccessToken(context.Background(), "Malc")
			if err != nil || got != "at-new" {
				t.Errorf("got %q, %v", got, err)
			}
		}()
	}
	wg.Wait()
	if f.count() != 1 {
		t.Errorf("Refresh calls = %d, want 1", f.count())
	}
}

func TestInvalidGrantMarksReconnect(t *testing.T) {
	f := &fakeRefresher{err: discord.ErrInvalidGrant}
	m, _ := setup(t, user(time.Minute), f)
	_, err := m.AccessToken(context.Background(), "Malc")
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("err = %v", err)
	}
	u, _, _ := m.Store.Get(context.Background(), "Malc")
	if !u.NeedsReconnect {
		t.Error("NeedsReconnect not set")
	}
	_, err = m.AccessToken(context.Background(), "Malc")
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("second err = %v", err)
	}
	if f.count() != 1 {
		t.Errorf("Refresh calls = %d, want 1", f.count())
	}
}

func TestTransientErrorDoesNotMarkReconnect(t *testing.T) {
	f := &fakeRefresher{err: errors.New("boom 503")}
	m, _ := setup(t, user(time.Minute), f)
	_, err := m.AccessToken(context.Background(), "Malc")
	if err == nil || errors.Is(err, ErrNotConnected) {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "rt-old") || strings.Contains(err.Error(), "at-old") {
		t.Errorf("token leaked: %v", err)
	}
	u, _, _ := m.Store.Get(context.Background(), "Malc")
	if u.NeedsReconnect || u.RefreshToken != "rt-old" {
		t.Errorf("row changed: %+v", u)
	}
}

func TestUnknownUser(t *testing.T) {
	f := &fakeRefresher{}
	m, _ := setup(t, nil, f)
	_, err := m.AccessToken(context.Background(), "nobody")
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("err = %v", err)
	}
	if f.count() != 0 {
		t.Error("Refresh called")
	}
}

func TestRefreshKeepsConcurrentPause(t *testing.T) {
	f := &fakeRefresher{resp: discord.TokenSet{AccessToken: "at-new", RefreshToken: "rt-new", ExpiresAt: t0.Add(time.Hour)}}
	u := user(time.Minute)
	u.Paused = false
	m, _ := setup(t, u, f)
	ctx := context.Background()
	f.during = func() {
		if err := m.Store.SetPaused(ctx, "Malc", true); err != nil {
			t.Error(err)
		}
	}
	if _, err := m.AccessToken(ctx, "Malc"); err != nil {
		t.Fatal(err)
	}
	got, _, _ := m.Store.Get(ctx, "Malc")
	if !got.Paused {
		t.Error("pause set during refresh was lost")
	}
	if got.RefreshToken != "rt-new" {
		t.Errorf("refresh token = %q, want rt-new", got.RefreshToken)
	}
}

func TestRefreshDoesNotResurrectDeletedRow(t *testing.T) {
	f := &fakeRefresher{resp: discord.TokenSet{AccessToken: "at-new", RefreshToken: "rt-new", ExpiresAt: t0.Add(time.Hour)}}
	m, _ := setup(t, user(time.Minute), f)
	ctx := context.Background()
	f.during = func() {
		if err := m.Store.Delete(ctx, "Malc"); err != nil {
			t.Error(err)
		}
	}
	if _, err := m.AccessToken(ctx, "Malc"); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("err = %v, want ErrNotConnected", err)
	}
	if _, ok, _ := m.Store.Get(ctx, "Malc"); ok {
		t.Fatal("deleted row re-inserted")
	}
}

func TestInvalidateForcesRefresh(t *testing.T) {
	f := &fakeRefresher{resp: discord.TokenSet{AccessToken: "at-new", RefreshToken: "rt-new", ExpiresAt: t0.Add(2 * time.Hour)}}
	m, _ := setup(t, user(time.Hour), f)
	ctx := context.Background()
	if err := m.Invalidate(ctx, "Malc"); err != nil {
		t.Fatal(err)
	}
	got, err := m.AccessToken(ctx, "Malc")
	if err != nil || got != "at-new" {
		t.Fatalf("got %q, %v", got, err)
	}
	if f.count() != 1 {
		t.Errorf("Refresh calls = %d, want 1", f.count())
	}
}
