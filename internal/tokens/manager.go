// Package tokens hands out fresh Discord access tokens, refreshing safely
// given that Discord rotates refresh tokens on every refresh.
package tokens

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/laforcem/nd-simple-rpc/internal/discord"
	"github.com/laforcem/nd-simple-rpc/internal/store"
)

// ErrNotConnected means the user has no usable Discord connection: no row,
// or the row needs a reconnect.
var ErrNotConnected = errors.New("tokens: user not connected to Discord")

const refreshWindow = 5 * time.Minute

// Refresher exchanges a refresh token for a new token set.
type Refresher interface {
	Refresh(ctx context.Context, refreshToken string) (discord.TokenSet, error)
}

// Manager serves access tokens, refreshing near expiry. Calls for the same
// user are serialized so a rotated refresh token is never used twice.
type Manager struct {
	Store   *store.Store
	Discord Refresher
	Now     func() time.Time

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func (m *Manager) userLock(username string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.locks == nil {
		m.locks = make(map[string]*sync.Mutex)
	}
	l, ok := m.locks[username]
	if !ok {
		l = &sync.Mutex{}
		m.locks[username] = l
	}
	return l
}

// AccessToken returns a valid access token for username, refreshing and
// persisting the rotated tokens first if it expires within five minutes.
// Returns ErrNotConnected if the user is unknown or needs to reconnect.
func (m *Manager) AccessToken(ctx context.Context, username string) (string, error) {
	l := m.userLock(username)
	l.Lock()
	defer l.Unlock()

	u, ok, err := m.Store.Get(ctx, username)
	if err != nil {
		return "", fmt.Errorf("tokens: load user: %w", err)
	}
	if !ok || u.NeedsReconnect {
		return "", ErrNotConnected
	}
	if u.AccessToken != "" && u.AccessExpiresAt.Sub(m.Now()) >= refreshWindow {
		return u.AccessToken, nil
	}

	ts, err := m.Discord.Refresh(ctx, u.RefreshToken)
	if err != nil {
		if errors.Is(err, discord.ErrInvalidGrant) {
			if merr := m.Store.MarkNeedsReconnect(ctx, username); merr != nil {
				return "", fmt.Errorf("tokens: mark reconnect: %w", merr)
			}
			return "", ErrNotConnected
		}
		return "", fmt.Errorf("tokens: refresh: %w", err)
	}
	if ts.RefreshToken == "" {
		ts.RefreshToken = u.RefreshToken
	}
	// Persist before returning: the old refresh token is now dead. Only the
	// token columns are written, so a concurrent pause is kept, and a row
	// deleted or flagged meanwhile is left alone.
	if err := m.Store.SetTokens(ctx, username, ts.AccessToken, ts.RefreshToken, ts.ExpiresAt); err != nil {
		if errors.Is(err, store.ErrNotUpdated) {
			return "", ErrNotConnected
		}
		return "", fmt.Errorf("tokens: persist refreshed tokens: %w", err)
	}
	return ts.AccessToken, nil
}

// Invalidate marks username's stored access token as expired, so the next
// AccessToken call refreshes it (for example after Discord answered 401).
func (m *Manager) Invalidate(ctx context.Context, username string) error {
	l := m.userLock(username)
	l.Lock()
	defer l.Unlock()
	if err := m.Store.ExpireAccessToken(ctx, username); err != nil {
		return fmt.Errorf("tokens: invalidate: %w", err)
	}
	return nil
}
