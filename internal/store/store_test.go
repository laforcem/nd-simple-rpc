package store

import (
	"bytes"
	"context"
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
