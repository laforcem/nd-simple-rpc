package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testKey(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path, testKey(1))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func TestPutGetRoundTrip(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	want := User{
		Username: "Malc", DiscordUserID: "123", DiscordUsername: "malc#1",
		RefreshToken: "rt-secret", AccessToken: "at-secret",
		AccessExpiresAt: time.Unix(1700000000, 123).UTC(),
		Paused:          true, SessionToken: "sess-secret", NeedsReconnect: true,
	}
	if err := s.Put(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Get(ctx, "Malc")
	if err != nil || !ok {
		t.Fatalf("Get ok=%v err=%v", ok, err)
	}
	if !got.AccessExpiresAt.Equal(want.AccessExpiresAt) {
		t.Errorf("expiry %v != %v", got.AccessExpiresAt, want.AccessExpiresAt)
	}
	got.AccessExpiresAt = want.AccessExpiresAt
	if got != want {
		t.Errorf("got %+v want %+v", got, want)
	}
	if _, ok, err := s.Get(ctx, "nobody"); ok || err != nil {
		t.Errorf("missing: ok=%v err=%v", ok, err)
	}
	// upsert
	want.Paused = false
	if err := s.Put(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.Get(ctx, "Malc")
	if got.Paused {
		t.Error("upsert did not update")
	}
	if err := s.Delete(ctx, "Malc"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Get(ctx, "Malc"); ok {
		t.Error("still present after Delete")
	}
}

func TestZeroExpiryRoundTrip(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	if err := s.Put(ctx, User{Username: "a"}); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.Get(ctx, "a")
	if !got.AccessExpiresAt.IsZero() {
		t.Errorf("expiry not zero: %v", got.AccessExpiresAt)
	}
}

func TestTokensEncryptedAtRest(t *testing.T) {
	s, path := openTemp(t)
	ctx := context.Background()
	err := s.Put(ctx, User{Username: "a", RefreshToken: "rt-secret", AccessToken: "at-secret", SessionToken: "sess-secret"})
	if err != nil {
		t.Fatal(err)
	}
	// Read before Close so any WAL content is included too.
	files, _ := filepath.Glob(path + "*")
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"rt-secret", "at-secret", "sess-secret"} {
			if bytes.Contains(raw, []byte(secret)) {
				t.Errorf("%s contains plaintext %q", f, secret)
			}
		}
	}
	s.Close()
	raw, _ := os.ReadFile(path)
	if bytes.Contains(raw, []byte("rt-secret")) {
		t.Error("plaintext in db after close")
	}
}

func TestWrongKeyFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "k.db")
	s, err := Open(path, testKey(1))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.Put(ctx, User{Username: "a", RefreshToken: "rt"}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2, err := Open(path, testKey(2))
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, _, err := s2.Get(ctx, "a"); err == nil {
		t.Fatal("expected error with wrong key")
	}
}

func TestMarkNeedsReconnect(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	s.Put(ctx, User{Username: "a", DiscordUserID: "9", RefreshToken: "rt", AccessToken: "at", SessionToken: "st"})
	if err := s.MarkNeedsReconnect(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Get(ctx, "a")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if got.RefreshToken != "" || got.AccessToken != "" || got.SessionToken != "" || !got.NeedsReconnect {
		t.Errorf("unexpected: %+v", got)
	}
	if got.DiscordUserID != "9" {
		t.Error("identity should be kept")
	}
}

func TestSetPausedAndSessionToken(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	s.Put(ctx, User{Username: "a", RefreshToken: "rt"})
	if err := s.SetPaused(ctx, "a", true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSessionToken(ctx, "a", "st"); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.Get(ctx, "a")
	if !got.Paused || got.SessionToken != "st" || got.RefreshToken != "rt" {
		t.Errorf("got %+v", got)
	}
	if err := s.SetSessionToken(ctx, "a", ""); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.Get(ctx, "a")
	if got.SessionToken != "" {
		t.Errorf("session not cleared: %q", got.SessionToken)
	}
}

func TestListWithSessions(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	s.Put(ctx, User{Username: "a", SessionToken: "s1"})
	s.Put(ctx, User{Username: "b"})
	s.Put(ctx, User{Username: "c", SessionToken: "s3"})
	got, err := s.ListWithSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, u := range got {
		names = append(names, u.Username)
	}
	if strings.Join(names, ",") != "a,c" {
		t.Errorf("got %v", names)
	}
	if got[0].SessionToken != "s1" {
		t.Errorf("token not decrypted: %q", got[0].SessionToken)
	}
}

func TestOpenRejectsShortKey(t *testing.T) {
	_, err := Open(filepath.Join(t.TempDir(), "x.db"), []byte("short"))
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestExpireAccessToken(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	exp := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	s.Put(ctx, User{Username: "a", RefreshToken: "rt", AccessToken: "at", AccessExpiresAt: exp, Paused: true, SessionToken: "st"})
	if err := s.ExpireAccessToken(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.Get(ctx, "a")
	if !got.AccessExpiresAt.IsZero() {
		t.Errorf("expiry = %v, want zero", got.AccessExpiresAt)
	}
	if got.RefreshToken != "rt" || !got.Paused || got.SessionToken != "st" {
		t.Errorf("other fields changed: %+v", got)
	}
	if err := s.ExpireAccessToken(ctx, "nobody"); err != nil {
		t.Errorf("absent user: %v", err)
	}
}

func TestSetTokens(t *testing.T) {
	s, path := openTemp(t)
	ctx := context.Background()
	exp := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	s.Put(ctx, User{Username: "a", DiscordUserID: "d", RefreshToken: "rt", AccessToken: "at", Paused: true, SessionToken: "st"})
	if err := s.SetTokens(ctx, "a", "at-new", "rt-new", exp); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.Get(ctx, "a")
	if got.AccessToken != "at-new" || got.RefreshToken != "rt-new" || !got.AccessExpiresAt.Equal(exp) {
		t.Errorf("tokens not stored: %+v", got)
	}
	if !got.Paused || got.SessionToken != "st" || got.DiscordUserID != "d" {
		t.Errorf("other fields changed: %+v", got)
	}
	files, _ := filepath.Glob(path + "*")
	for _, f := range files {
		raw, _ := os.ReadFile(f)
		if bytes.Contains(raw, []byte("rt-new")) || bytes.Contains(raw, []byte("at-new")) {
			t.Errorf("%s holds plaintext tokens", f)
		}
	}

	if err := s.SetTokens(ctx, "nobody", "x", "y", exp); !errors.Is(err, ErrNotUpdated) {
		t.Errorf("absent user err = %v, want ErrNotUpdated", err)
	}
	if _, ok, _ := s.Get(ctx, "nobody"); ok {
		t.Error("absent user inserted")
	}

	s.MarkNeedsReconnect(ctx, "a")
	if err := s.SetTokens(ctx, "a", "x", "y", exp); !errors.Is(err, ErrNotUpdated) {
		t.Errorf("needs-reconnect err = %v, want ErrNotUpdated", err)
	}
	if got, _, _ := s.Get(ctx, "a"); got.RefreshToken != "" || !got.NeedsReconnect {
		t.Errorf("needs-reconnect row changed: %+v", got)
	}
}

func TestSessionGeneration(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	if g, err := s.SessionGeneration(ctx, "Malc"); err != nil || g != 0 {
		t.Fatalf("fresh: %d, %v", g, err)
	}
	for want := int64(1); want <= 2; want++ {
		if err := s.BumpSessionGeneration(ctx, "Malc"); err != nil {
			t.Fatal(err)
		}
		if g, _ := s.SessionGeneration(ctx, "Malc"); g != want {
			t.Fatalf("after bump: %d want %d", g, want)
		}
	}
	// Deleting the user row must not reset the generation.
	if err := s.Delete(ctx, "Malc"); err != nil {
		t.Fatal(err)
	}
	if g, _ := s.SessionGeneration(ctx, "Malc"); g != 2 {
		t.Fatalf("after delete: %d", g)
	}
	if g, _ := s.SessionGeneration(ctx, "Other"); g != 0 {
		t.Fatalf("other user: %d", g)
	}
}
