// Package store persists per-user Discord credentials in SQLite, with
// tokens encrypted at rest using AES-256-GCM.
package store

import (
	"context"
	"crypto/cipher"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotUpdated means a narrow update matched no row: the user is absent or,
// for SetTokens, flagged for reconnection.
var ErrNotUpdated = errors.New("store: no matching user row")

// User is one linked Navidrome user. Tokens are plaintext in memory.
type User struct {
	Username        string
	DiscordUserID   string
	DiscordUsername string
	RefreshToken    string
	AccessToken     string
	AccessExpiresAt time.Time
	Paused          bool
	SessionToken    string
	NeedsReconnect  bool
}

// Store is a SQLite-backed user store.
type Store struct {
	db   *sql.DB
	aead cipher.AEAD
}

const schema = `CREATE TABLE IF NOT EXISTS users (
	username          TEXT PRIMARY KEY,
	discord_user_id   TEXT NOT NULL DEFAULT '',
	discord_username  TEXT NOT NULL DEFAULT '',
	refresh_token     BLOB NOT NULL DEFAULT x'',
	access_token      BLOB NOT NULL DEFAULT x'',
	access_expires_at INTEGER NOT NULL DEFAULT 0,
	paused            INTEGER NOT NULL DEFAULT 0,
	session_token     BLOB NOT NULL DEFAULT x'',
	needs_reconnect   INTEGER NOT NULL DEFAULT 0
)`

// web_sessions outlives users rows: Delete must not reset a generation and
// revive old cookies.
const sessionSchema = `CREATE TABLE IF NOT EXISTS web_sessions (
	username   TEXT PRIMARY KEY,
	generation INTEGER NOT NULL DEFAULT 0
)`

const cols = `username, discord_user_id, discord_username, refresh_token, access_token,
	access_expires_at, paused, session_token, needs_reconnect`

// Open opens (creating if needed) the database at path. key must be 32 bytes.
func Open(path string, key []byte) (*Store, error) {
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: create schema: %w", err)
	}
	if _, err := db.Exec(sessionSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: create schema: %w", err)
	}
	return &Store{db: db, aead: aead}, nil
}

func (s *Store) Close() error { return s.db.Close() }

type scanner interface{ Scan(...any) error }

func (s *Store) scan(r scanner) (User, error) {
	var (
		u              User
		rt, at, sess   []byte
		exp            int64
		paused, reconn int
	)
	if err := r.Scan(&u.Username, &u.DiscordUserID, &u.DiscordUsername, &rt, &at, &exp, &paused, &sess, &reconn); err != nil {
		return User{}, err
	}
	var err error
	if u.RefreshToken, err = open(s.aead, rt); err != nil {
		return User{}, err
	}
	if u.AccessToken, err = open(s.aead, at); err != nil {
		return User{}, err
	}
	if u.SessionToken, err = open(s.aead, sess); err != nil {
		return User{}, err
	}
	if exp != 0 {
		u.AccessExpiresAt = time.Unix(0, exp).UTC()
	}
	u.Paused = paused != 0
	u.NeedsReconnect = reconn != 0
	return u, nil
}

// Get returns the user, or ok=false if absent.
func (s *Store) Get(ctx context.Context, username string) (User, bool, error) {
	u, err := s.scan(s.db.QueryRowContext(ctx, `SELECT `+cols+` FROM users WHERE username = ?`, username))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, false, nil
	}
	if err != nil {
		return User{}, false, err
	}
	return u, true, nil
}

// Put upserts by username.
func (s *Store) Put(ctx context.Context, u User) error {
	rt, err := seal(s.aead, u.RefreshToken)
	if err != nil {
		return err
	}
	at, err := seal(s.aead, u.AccessToken)
	if err != nil {
		return err
	}
	sess, err := seal(s.aead, u.SessionToken)
	if err != nil {
		return err
	}
	var exp int64
	if !u.AccessExpiresAt.IsZero() {
		exp = u.AccessExpiresAt.UnixNano()
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO users (`+cols+`) VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(username) DO UPDATE SET
			discord_user_id=excluded.discord_user_id,
			discord_username=excluded.discord_username,
			refresh_token=excluded.refresh_token,
			access_token=excluded.access_token,
			access_expires_at=excluded.access_expires_at,
			paused=excluded.paused,
			session_token=excluded.session_token,
			needs_reconnect=excluded.needs_reconnect`,
		u.Username, u.DiscordUserID, u.DiscordUsername, rt, at, exp, b2i(u.Paused), sess, b2i(u.NeedsReconnect))
	return err
}

// Delete removes the user; deleting an absent user is not an error.
func (s *Store) Delete(ctx context.Context, username string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE username = ?`, username)
	return err
}

func (s *Store) SetPaused(ctx context.Context, username string, paused bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET paused = ? WHERE username = ?`, b2i(paused), username)
	return err
}

// SetSessionToken stores the Discord headless-session token; "" clears it.
func (s *Store) SetSessionToken(ctx context.Context, username, token string) error {
	blob, err := seal(s.aead, token)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE users SET session_token = ? WHERE username = ?`, blob, username)
	return err
}

// SetTokens stores a refreshed token set without touching any other column.
// It only updates a row that exists and is not flagged for reconnection,
// returning ErrNotUpdated otherwise, so a refresh racing a disconnect or a
// reconnect flag never resurrects or overwrites that row.
func (s *Store) SetTokens(ctx context.Context, username, access, refresh string, exp time.Time) error {
	at, err := seal(s.aead, access)
	if err != nil {
		return err
	}
	rt, err := seal(s.aead, refresh)
	if err != nil {
		return err
	}
	var e int64
	if !exp.IsZero() {
		e = exp.UnixNano()
	}
	res, err := s.db.ExecContext(ctx, `UPDATE users SET access_token = ?, refresh_token = ?, access_expires_at = ?
		WHERE username = ? AND needs_reconnect = 0`, at, rt, e, username)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotUpdated
	}
	return nil
}

// ExpireAccessToken marks the stored access token as expired, so the next
// AccessToken call refreshes it.
func (s *Store) ExpireAccessToken(ctx context.Context, username string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET access_expires_at = 0 WHERE username = ?`, username)
	return err
}

// MarkNeedsReconnect clears all tokens and flags the user for reconnection.
func (s *Store) MarkNeedsReconnect(ctx context.Context, username string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET refresh_token = x'', access_token = x'',
		access_expires_at = 0, session_token = x'', needs_reconnect = 1 WHERE username = ?`, username)
	return err
}

// ListWithSessions returns users that currently hold a session token.
func (s *Store) ListWithSessions(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+cols+` FROM users WHERE length(session_token) > 0 ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := s.scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SessionGeneration returns the user's web session generation; 0 if never bumped.
func (s *Store) SessionGeneration(ctx context.Context, username string) (int64, error) {
	var g int64
	err := s.db.QueryRowContext(ctx, `SELECT generation FROM web_sessions WHERE username = ?`, username).Scan(&g)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return g, err
}

// BumpSessionGeneration invalidates every web session issued to the user.
func (s *Store) BumpSessionGeneration(ctx context.Context, username string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO web_sessions (username, generation) VALUES (?, 1)
		ON CONFLICT(username) DO UPDATE SET generation = generation + 1`, username)
	return err
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
