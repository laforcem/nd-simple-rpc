package discord

import (
	"errors"
	"fmt"
	"time"
)

var (
	// ErrInvalidGrant: the token endpoint rejected the code or refresh token.
	ErrInvalidGrant = errors.New("discord: invalid_grant")
	// ErrSessionGone: a session endpoint answered 400; the session is unknown or expired.
	ErrSessionGone = errors.New("discord: session gone")
	// ErrUnauthorized: Discord answered 401.
	ErrUnauthorized = errors.New("discord: unauthorized")
)

// RateLimitError is a 429 response.
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("discord: rate limited, retry after %s", e.RetryAfter)
}

// statusError is any other unexpected status. It carries no response body,
// so it can never leak a token a server echoes back.
type statusError struct {
	op     string
	status int
}

func (e *statusError) Error() string {
	return fmt.Sprintf("discord: %s: unexpected status %d", e.op, e.status)
}
