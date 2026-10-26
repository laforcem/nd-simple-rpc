// Package engine turns playback events into Discord headless-session calls.
// HandlePlayback only records what each user should be showing; Tick
// reconciles that with what Discord shows, at most one call per user per
// tick and within the per-user rate limit, which coalesces rapid changes.
package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/laforcem/nd-simple-rpc/internal/activity"
	"github.com/laforcem/nd-simple-rpc/internal/discord"
	"github.com/laforcem/nd-simple-rpc/internal/event"
	"github.com/laforcem/nd-simple-rpc/internal/store"
	"github.com/laforcem/nd-simple-rpc/internal/tokens"
)

// Users is the subset of the user store the engine needs.
type Users interface {
	Get(ctx context.Context, username string) (store.User, bool, error)
	SetSessionToken(ctx context.Context, username, token string) error
	ListWithSessions(ctx context.Context) ([]store.User, error)
}

// Tokens hands out Discord access tokens.
type Tokens interface {
	AccessToken(ctx context.Context, username string) (string, error)
	// Invalidate forces the next AccessToken call to refresh.
	Invalidate(ctx context.Context, username string) error
}

// Publisher creates, updates and deletes headless sessions.
type Publisher interface {
	PutSession(ctx context.Context, accessToken, sessionToken string, a activity.Activity) (string, error)
	DeleteSession(ctx context.Context, accessToken, sessionToken string) error
}

// Config tunes the engine. Zero values take the defaults noted.
type Config struct {
	AppID      string
	Keepalive  time.Duration // 15m
	Grace      time.Duration // 30s
	Tolerance  time.Duration // 3s
	RateMax    int           // 4
	RateWindow time.Duration // 20s
	Logger     *slog.Logger  // nil discards
}

type op int

const (
	opNone op = iota
	opPut
	opDelete
)

const (
	errBackoffMin = 2 * time.Second
	errBackoffMax = 5 * time.Minute
)

// errorBackoff is the wait after the step-th consecutive failed call:
// 2 s doubling to a 5 min cap.
func errorBackoff(step int) time.Duration {
	d := errBackoffMin
	for i := 1; i < step && d < errBackoffMax; i++ {
		d *= 2
	}
	return min(d, errBackoffMax)
}

func (o op) String() string {
	switch o {
	case opPut:
		return "put"
	case opDelete:
		return "delete"
	}
	return "none"
}

// userState is guarded by Engine.mu, except op, which serializes Discord
// calls for the user (Tick, Clear, Startup) without holding Engine.mu.
type userState struct {
	op sync.Mutex

	desired     *activity.Activity // nil: show nothing
	expectedEnd time.Time          // desired goes stale after this
	gen         uint64             // bumped by every event

	sessionKnown bool   // session has been loaded from the store
	session      string // mirror of the stored session token
	posted       *activity.Activity
	lastPut      time.Time
	backoffUntil time.Time // 429 Retry-After
	lim          limiter

	errStep     int       // consecutive failed calls (non-429)
	errUntil    time.Time // no reconcile call before this
	rejectedGen uint64    // Discord rejected the create for this gen; 0: none
}

// Engine is the presence reconciler. Safe for concurrent use.
type Engine struct {
	cfg    Config
	users  Users
	tokens Tokens
	pub    Publisher
	now    func() time.Time
	log    *slog.Logger

	mu    sync.Mutex
	state map[string]*userState
}

// New builds an engine. now is the clock (time.Now in production).
func New(cfg Config, u Users, t Tokens, p Publisher, now func() time.Time) *Engine {
	if cfg.Keepalive <= 0 {
		cfg.Keepalive = 15 * time.Minute
	}
	if cfg.Grace <= 0 {
		cfg.Grace = 30 * time.Second
	}
	if cfg.Tolerance <= 0 {
		cfg.Tolerance = 3 * time.Second
	}
	if cfg.RateMax <= 0 {
		cfg.RateMax = 4
	}
	if cfg.RateWindow <= 0 {
		cfg.RateWindow = 20 * time.Second
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Engine{cfg: cfg, users: u, tokens: t, pub: p, now: now, log: log, state: map[string]*userState{}}
}

// HandlePlayback records the desired state for ev.Username. It never calls
// Discord.
func (e *Engine) HandlePlayback(ev event.Playback) {
	var desired *activity.Activity
	var end time.Time
	if event.Active(ev.State) {
		a := activity.Build(ev, e.cfg.AppID)
		desired = &a
		rate := ev.PlaybackRate
		if rate <= 0 {
			rate = 1
		}
		remaining := time.Duration(float64(ev.Track.DurationMs-ev.PositionMs)/rate) * time.Millisecond
		end = time.Unix(ev.Timestamp, 0).Add(remaining + e.cfg.Grace)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.state[ev.Username]
	if st == nil {
		st = &userState{}
		e.state[ev.Username] = st
	}
	st.desired = desired
	st.expectedEnd = end
	st.gen++
	st.errStep, st.errUntil = 0, time.Time{}
}

// Tick reconciles every known user once.
func (e *Engine) Tick(ctx context.Context) {
	e.mu.Lock()
	names := make([]string, 0, len(e.state))
	for n := range e.state {
		names = append(names, n)
	}
	e.mu.Unlock()
	for _, n := range names {
		if ctx.Err() != nil {
			return
		}
		e.reconcile(ctx, n)
	}
}

// Run calls Tick every second until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.Tick(ctx)
		}
	}
}

// Clear deletes the user's live session now, ignoring the rolling window and
// the error back-off but honouring a pending 429 back-off. It keeps the
// desired activity: callers set the user's Paused flag first, which keeps
// Tick from re-posting (and makes it retry a failed delete) until resumed.
func (e *Engine) Clear(ctx context.Context, username string) error {
	st := e.lockUser(username, true)
	defer st.op.Unlock()

	e.mu.Lock()
	known := st.sessionKnown
	e.mu.Unlock()

	if !known {
		u, ok, err := e.users.Get(ctx, username)
		if err != nil {
			return err
		}
		e.mu.Lock()
		if !st.sessionKnown {
			st.session, st.sessionKnown = "", true
			if ok {
				st.session = u.SessionToken
			}
		}
		e.mu.Unlock()
	}

	e.mu.Lock()
	session, gen, now := st.session, st.gen, e.now()
	if session == "" {
		e.mu.Unlock()
		return nil
	}
	if now.Before(st.backoffUntil) {
		wait := st.backoffUntil.Sub(now)
		e.mu.Unlock()
		return &discord.RateLimitError{RetryAfter: wait}
	}
	e.mu.Unlock()
	return e.call(ctx, username, st, opDelete, session, nil, gen)
}

// Startup deletes every stored session and clears it. A delete that fails
// transiently is left queued for Tick to retry; only a failure to list the
// stored sessions is returned.
func (e *Engine) Startup(ctx context.Context) error {
	us, err := e.users.ListWithSessions(ctx)
	if err != nil {
		return err
	}
	for _, u := range us {
		st := e.lockUser(u.Username, true)
		e.mu.Lock()
		st.session, st.sessionKnown, st.posted = u.SessionToken, true, nil
		gen := st.gen
		e.mu.Unlock()
		err := e.call(ctx, u.Username, st, opDelete, u.SessionToken, nil, gen)
		if errors.Is(err, tokens.ErrNotConnected) {
			// Unreachable at Discord; it expires there on its own.
			e.persist(ctx, u.Username, "")
		}
		st.op.Unlock()
	}
	return nil
}

// lockUser returns the user's state with its op lock held, creating the
// state if create is set (otherwise nil if absent). It re-checks after
// locking so it never returns a state that was pruned meanwhile.
func (e *Engine) lockUser(name string, create bool) *userState {
	for {
		e.mu.Lock()
		st := e.state[name]
		if st == nil {
			if !create {
				e.mu.Unlock()
				return nil
			}
			st = &userState{}
			e.state[name] = st
		}
		e.mu.Unlock()
		st.op.Lock()
		e.mu.Lock()
		current := e.state[name] == st
		e.mu.Unlock()
		if current {
			return st
		}
		st.op.Unlock()
	}
}

func (e *Engine) reconcile(ctx context.Context, name string) {
	st := e.lockUser(name, false)
	if st == nil {
		return
	}
	defer st.op.Unlock()

	e.mu.Lock()
	gen := st.gen
	e.mu.Unlock()

	u, ok, err := e.users.Get(ctx, name)
	if err != nil {
		e.log.Warn("presence: load user failed", "user", name, "err", err)
		return
	}

	e.mu.Lock()
	if !ok {
		// No row: nothing can be posted or deleted for this user.
		e.dropLocked(st, gen)
		e.pruneLocked(name, st, e.now())
		e.mu.Unlock()
		return
	}
	if !st.sessionKnown {
		st.session, st.sessionKnown = u.SessionToken, true
	}
	now := e.now()
	if st.desired != nil && now.After(st.expectedEnd) {
		st.desired = nil
	}
	want := st.desired
	if u.Paused || u.NeedsReconnect {
		want = nil
	}
	o, act := e.decideLocked(st, want, now)
	if o == opNone {
		e.pruneLocked(name, st, now)
		e.mu.Unlock()
		return
	}
	if now.Before(st.backoffUntil) || now.Before(st.errUntil) ||
		!st.lim.allow(now, e.cfg.RateMax, e.cfg.RateWindow) {
		e.mu.Unlock()
		return
	}
	session := st.session
	e.mu.Unlock()

	err = e.call(ctx, name, st, o, session, act, gen)
	var rl *discord.RateLimitError
	if err == nil || errors.Is(err, tokens.ErrNotConnected) || errors.Is(err, discord.ErrSessionGone) ||
		errors.As(err, &rl) {
		return
	}
	// Anything else (401, 403, 5xx, transport, refresh failure) backs off.
	// One warning per step; err never carries tokens or URLs.
	e.mu.Lock()
	st.errStep++
	wait := errorBackoff(st.errStep)
	st.errUntil = e.now().Add(wait)
	e.mu.Unlock()
	e.log.Warn("presence: discord call failed, backing off", "user", name, "op", o.String(),
		"retry_in", wait.String(), "err", err)
}

// decideLocked picks the one call (if any) that moves Discord towards want.
func (e *Engine) decideLocked(st *userState, want *activity.Activity, now time.Time) (op, *activity.Activity) {
	if want != nil && st.rejectedGen != 0 && st.rejectedGen == st.gen {
		// Discord rejected this payload (and holds no session for it, since
		// only a create can be rejected); wait for the next event.
		return opNone, nil
	}
	switch {
	case want != nil && st.session == "":
		return opPut, want
	case want != nil && (st.posted == nil || activity.Changed(*st.posted, *want, e.cfg.Tolerance)):
		return opPut, want
	case want != nil && now.Sub(st.lastPut) >= e.cfg.Keepalive:
		return opPut, want
	case want == nil && st.session != "":
		return opDelete, nil
	}
	return opNone, nil
}

// call performs one Discord call with st.op held and applies its outcome.
// A delete answered with ErrSessionGone counts as success.
func (e *Engine) call(ctx context.Context, name string, st *userState, o op, session string, act *activity.Activity, gen uint64) error {
	access, err := e.tokens.AccessToken(ctx, name)
	if err != nil {
		if errors.Is(err, tokens.ErrNotConnected) {
			e.mu.Lock()
			e.dropLocked(st, gen)
			e.mu.Unlock()
		}
		return err
	}

	e.mu.Lock()
	st.lim.record(e.now(), e.cfg.RateMax)
	e.mu.Unlock()

	var tok string
	if o == opDelete {
		err = e.pub.DeleteSession(ctx, access, session)
	} else {
		tok, err = e.pub.PutSession(ctx, access, session, *act)
	}

	var rl *discord.RateLimitError
	switch {
	case err == nil && o == opPut:
		e.mu.Lock()
		st.session, st.posted, st.lastPut = tok, act, e.now()
		st.errStep, st.errUntil = 0, time.Time{}
		e.mu.Unlock()
		e.persist(ctx, name, tok)
	case o == opPut && session == "" && errors.Is(err, discord.ErrSessionGone):
		// A create has no session to lose: the 400 rejects the payload.
		e.mu.Lock()
		st.rejectedGen = gen
		e.mu.Unlock()
		e.log.Warn("presence: discord rejected the activity; not retrying until the next playback event",
			"user", name, "status", "400 Bad Request")
	case err == nil || errors.Is(err, discord.ErrSessionGone):
		e.mu.Lock()
		st.session, st.posted = "", nil
		if err == nil {
			st.errStep, st.errUntil = 0, time.Time{}
		}
		e.mu.Unlock()
		e.persist(ctx, name, "")
		if o == opDelete {
			err = nil
		}
	case errors.As(err, &rl):
		e.mu.Lock()
		st.backoffUntil = e.now().Add(rl.RetryAfter)
		e.mu.Unlock()
	case errors.Is(err, discord.ErrUnauthorized):
		// The access token may have been revoked or rotated early; make the
		// next attempt refresh, which recovers or ends in NeedsReconnect.
		if ierr := e.tokens.Invalidate(ctx, name); ierr != nil {
			e.log.Warn("presence: invalidate access token failed", "user", name, "err", ierr)
		}
	}
	return err
}

// dropLocked forgets in-memory session state; the session is reloaded from
// the store next time. desired is dropped only if no event arrived since gen.
func (e *Engine) dropLocked(st *userState, gen uint64) {
	st.session, st.sessionKnown, st.posted = "", false, nil
	if st.gen == gen {
		st.desired = nil
	}
}

// pruneLocked removes an idle user's state once nothing it holds matters,
// including recent calls the rate limiter must still count.
func (e *Engine) pruneLocked(name string, st *userState, now time.Time) {
	if st.desired == nil && st.session == "" && !now.Before(st.backoffUntil) &&
		st.lim.idle(now, e.cfg.RateWindow) {
		delete(e.state, name)
	}
}

func (e *Engine) persist(ctx context.Context, name, tok string) {
	if err := e.users.SetSessionToken(ctx, name, tok); err != nil {
		e.log.Warn("presence: store session token failed", "user", name, "err", err)
	}
}
