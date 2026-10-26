// Package web serves the public pages where users sign in with their
// Navidrome login, connect Discord, and pause or disconnect.
package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"errors"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/laforcem/nd-simple-rpc/internal/discord"
	"github.com/laforcem/nd-simple-rpc/internal/navidrome"
	"github.com/laforcem/nd-simple-rpc/internal/store"
	"github.com/laforcem/nd-simple-rpc/internal/tokens"
)

//go:embed templates/*.html
var templateFS embed.FS

var tmpl = template.Must(template.ParseFS(templateFS, "templates/*.html"))

// DiscordClient is the subset of *discord.Client the web handlers use.
type DiscordClient interface {
	AuthorizeURL(state, codeChallenge string) string
	ExchangeCode(ctx context.Context, code, verifier string) (discord.TokenSet, error)
	CurrentUser(ctx context.Context, accessToken string) (discord.User, error)
	Revoke(ctx context.Context, token string) error
}

// Clearer ends a user's live Discord presence.
type Clearer interface {
	Clear(ctx context.Context, username string) error
}

// Deps are the handler dependencies.
type Deps struct {
	Store        *store.Store
	Discord      DiscordClient
	Engine       Clearer
	NavidromeURL string
	PublicURL    string // public base URL of this service (the OAuth redirect URI is configured on the Discord client)
	SessionKey   []byte
	// TrustedProxyHops is how many reverse proxies append to
	// X-Forwarded-For; 0 ignores the header and uses the peer address.
	TrustedProxyHops int
	Now              func() time.Time // nil uses time.Now
	Log              *slog.Logger     // nil uses slog.Default()
}

const (
	loginMaxFailures = 5
	loginWindow      = 15 * time.Minute
	failuresSweepAt  = 1024  // sweep stale entries on insert above this many keys
	failuresMaxKeys  = 10000 // hard cap; the oldest entry is dropped
)

type server struct {
	Deps
	key []byte
	log *slog.Logger

	mu       sync.Mutex
	failures map[string][]time.Time
	// floor is at or before every key's latest failure; a sweep can only
	// remove something once it falls outside the window.
	floor time.Time
}

// NewPublic returns the handler for the public site.
func NewPublic(d Deps) (http.Handler, error) {
	if len(d.SessionKey) < 32 {
		return nil, errors.New("web: SessionKey must be at least 32 bytes")
	}
	s := &server{Deps: d, failures: map[string][]time.Time{}}
	// Derive a purpose-specific key so the raw SessionKey is never used directly.
	k := sha256.Sum256(append([]byte("nd-simple-rpc/web/v1\x00"), d.SessionKey...))
	s.key = k[:]
	s.log = d.Log
	if s.log == nil {
		s.log = slog.Default()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("GET /settings", s.auth(s.settings))
	mux.HandleFunc("GET /connect", s.auth(s.connect))
	mux.HandleFunc("GET /callback", s.auth(s.callback))
	mux.HandleFunc("POST /pause", s.authPost(s.pause))
	mux.HandleFunc("POST /resume", s.authPost(s.resume))
	mux.HandleFunc("POST /disconnect", s.authPost(s.disconnect))
	mux.HandleFunc("POST /logout", s.authPost(s.logout))
	return noStore(mux), nil
}

func noStore(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		h.ServeHTTP(w, r)
	})
}

func (s *server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

type handler func(w http.ResponseWriter, r *http.Request, user string)

func (s *server) currentUser(r *http.Request) (user, sessionValue string, ok bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return "", "", false
	}
	user, ok = s.sessionUser(c.Value)
	return user, c.Value, ok
}

func (s *server) auth(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, _, ok := s.currentUser(r)
		if !ok {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		h(w, r, user)
	}
}

// authPost requires a session and a valid CSRF token.
func (s *server) authPost(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.sameOrigin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		user, sv, ok := s.currentUser(r)
		if !ok {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		if err := r.ParseForm(); err != nil || !s.csrfOK(sv, r.PostFormValue("csrf")) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h(w, r, user)
	}
}

func (s *server) render(w http.ResponseWriter, status int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		s.log.Error("render template", "template", name, "err", err)
	}
}

// sameOrigin rejects cross-site POSTs: browsers send Sec-Fetch-Site and Origin
// on them; non-browser clients that send neither are not a CSRF vector.
func (s *server) sameOrigin(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	if o := r.Header.Get("Origin"); o != "" {
		pu, err := url.Parse(s.PublicURL)
		if err != nil || o != pu.Scheme+"://"+pu.Host {
			return false
		}
	}
	return true
}

func (s *server) setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
}

func (s *server) index(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.currentUser(r); ok {
		http.Redirect(w, r, "/settings", http.StatusFound)
		return
	}
	s.render(w, http.StatusOK, "login.html", map[string]any{})
}

// clientIP returns the X-Forwarded-For entry hops from the right: each
// trusted proxy appends the address it saw, so entries left of that are
// client-supplied and untrusted. With hops 0, or fewer entries than hops,
// it uses the peer address.
func clientIP(r *http.Request, hops int) string {
	if hops > 0 {
		var parts []string
		for _, h := range r.Header.Values("X-Forwarded-For") {
			parts = append(parts, strings.Split(h, ",")...)
		}
		if len(parts) >= hops {
			if ip := strings.TrimSpace(parts[len(parts)-hops]); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// recent returns the in-window failures for ip, pruning old ones. Caller holds mu.
func (s *server) recent(ip string) []time.Time {
	cutoff := s.now().Add(-loginWindow)
	kept := s.failures[ip][:0]
	for _, t := range s.failures[ip] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(s.failures, ip)
		return nil
	}
	s.failures[ip] = kept
	return kept
}

// addFailure records a failed (or reserved) attempt for ip, bounding the
// map: above failuresSweepAt keys it first drops entries with nothing in
// the window, and at failuresMaxKeys it drops the key whose latest attempt
// is oldest. Caller holds mu.
func (s *server) addFailure(ip string, t time.Time) {
	if _, ok := s.failures[ip]; !ok {
		if len(s.failures) > failuresSweepAt && !s.floor.After(s.now().Add(-loginWindow)) {
			floor := t
			for k := range s.failures {
				if kept := s.recent(k); len(kept) > 0 && kept[len(kept)-1].Before(floor) {
					floor = kept[len(kept)-1]
				}
			}
			s.floor = floor
		}
		if len(s.failures) >= failuresMaxKeys {
			var oldestKey string
			var oldest time.Time
			for k, v := range s.failures {
				if last := v[len(v)-1]; oldestKey == "" || last.Before(oldest) {
					oldestKey, oldest = k, last
				}
			}
			delete(s.failures, oldestKey)
		}
	}
	if len(s.failures) == 0 || t.Before(s.floor) {
		s.floor = t
	}
	s.failures[ip] = append(s.failures[ip], t)
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	ip := clientIP(r, s.TrustedProxyHops)
	// Reserve a slot before calling Navidrome so concurrent requests cannot
	// all slip under the limit. It is released unless the attempt fails on bad credentials.
	s.mu.Lock()
	if len(s.recent(ip)) >= loginMaxFailures {
		s.mu.Unlock()
		s.render(w, http.StatusTooManyRequests, "login.html", map[string]any{"Error": "Too many failed attempts. Try again later."})
		return
	}
	reserved := s.now()
	s.addFailure(ip, reserved)
	s.mu.Unlock()
	release := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		f := s.failures[ip]
		for i := len(f) - 1; i >= 0; i-- {
			if f[i].Equal(reserved) {
				s.failures[ip] = append(f[:i], f[i+1:]...)
				break
			}
		}
		if len(s.failures[ip]) == 0 {
			delete(s.failures, ip)
		}
	}
	if err := r.ParseForm(); err != nil {
		release()
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	user, pass := r.PostFormValue("username"), r.PostFormValue("password")
	canonical, err := navidrome.Login(r.Context(), s.NavidromeURL, user, pass)
	switch {
	case errors.Is(err, navidrome.ErrBadCredentials):
		s.render(w, http.StatusOK, "login.html", map[string]any{"Error": "Invalid username or password."})
		return
	case err != nil:
		release()
		s.log.Error("navidrome login failed", "err", err)
		s.render(w, http.StatusBadGateway, "login.html", map[string]any{"Error": "Could not reach Navidrome. Try again later."})
		return
	}
	release()
	s.setCookie(w, sessionCookie, s.sessionValue(canonical), int(sessionTTL/time.Second))
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

func (s *server) settings(w http.ResponseWriter, r *http.Request, user string) {
	u, _, err := s.Store.Get(r.Context(), user)
	if err != nil {
		s.log.Error("settings: store get", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	c, _ := r.Cookie(sessionCookie)
	s.render(w, http.StatusOK, "settings.html", map[string]any{
		"Username":        user,
		"Connected":       u.DiscordUserID != "" && !u.NeedsReconnect,
		"NeedsReconnect":  u.NeedsReconnect,
		"DiscordUsername": u.DiscordUsername,
		"Paused":          u.Paused,
		"CSRF":            s.csrf(c.Value),
	})
}

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return b64.EncodeToString(b), nil
}

func (s *server) connect(w http.ResponseWriter, r *http.Request, user string) {
	state, err1 := randomString(16)
	verifier, err2 := randomString(32)
	if err1 != nil || err2 != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	sum := sha256.Sum256([]byte(verifier))
	s.setCookie(w, oauthCookie, s.oauthValue(state, verifier, user), int(oauthTTL/time.Second))
	http.Redirect(w, r, s.Discord.AuthorizeURL(state, b64.EncodeToString(sum[:])), http.StatusFound)
}

func (s *server) callback(w http.ResponseWriter, r *http.Request, user string) {
	c, err := r.Cookie(oauthCookie)
	if err != nil {
		http.Error(w, "missing or expired authorization state", http.StatusBadRequest)
		return
	}
	state, verifier, boundUser, ok := s.oauthParts(c.Value)
	got := r.URL.Query().Get("state")
	if !ok || boundUser != user || subtle.ConstantTimeCompare([]byte(state), []byte(got)) != 1 {
		http.Error(w, "invalid authorization state", http.StatusBadRequest)
		return
	}
	s.setCookie(w, oauthCookie, "", -1) // single use
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "Discord authorization was not granted", http.StatusBadRequest)
		return
	}
	ts, err := s.Discord.ExchangeCode(r.Context(), code, verifier)
	if err != nil {
		s.log.Error("callback: exchange code", "user", user, "err", err)
		http.Error(w, "could not complete Discord authorization", http.StatusBadGateway)
		return
	}
	du, err := s.Discord.CurrentUser(r.Context(), ts.AccessToken)
	if err != nil {
		s.log.Error("callback: current user", "user", user, "err", err)
		http.Error(w, "could not complete Discord authorization", http.StatusBadGateway)
		return
	}
	old, _, err := s.Store.Get(r.Context(), user)
	if err != nil {
		s.log.Error("callback: store get", "user", user, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Best effort: a replaced grant for another Discord account would
	// otherwise stay valid. Not for the same account: Discord's revoke
	// invalidates every token the app holds for that user, the new ones too.
	if old.RefreshToken != "" && old.RefreshToken != ts.RefreshToken && old.DiscordUserID != du.ID {
		if err := s.Discord.Revoke(r.Context(), old.RefreshToken); err != nil {
			s.log.Warn("callback: revoke replaced grant", "user", user, "err", err)
		}
	}
	if err := s.Store.Put(r.Context(), store.User{
		Username: user, DiscordUserID: du.ID, DiscordUsername: du.Username,
		RefreshToken: ts.RefreshToken, AccessToken: ts.AccessToken, AccessExpiresAt: ts.ExpiresAt,
		Paused: old.Paused,
	}); err != nil {
		s.log.Error("callback: store put", "user", user, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

func (s *server) pause(w http.ResponseWriter, r *http.Request, user string) {
	// Set the flag first so the reconciler cannot re-post after Clear.
	if err := s.Store.SetPaused(r.Context(), user, true); err != nil {
		s.log.Error("pause: set paused", "user", user, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := s.Engine.Clear(r.Context(), user); err != nil && !errors.Is(err, tokens.ErrNotConnected) {
		s.log.Error("pause: clear", "user", user, "err", err)
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

func (s *server) resume(w http.ResponseWriter, r *http.Request, user string) {
	if err := s.Store.SetPaused(r.Context(), user, false); err != nil {
		s.log.Error("resume: set paused", "user", user, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

func (s *server) disconnect(w http.ResponseWriter, r *http.Request, user string) {
	// Pause first so the reconciler cannot re-post after Clear, and read the
	// row after Clear: a refresh during Clear rotates the refresh token.
	if err := s.Store.SetPaused(r.Context(), user, true); err != nil {
		s.log.Error("disconnect: set paused", "user", user, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := s.Engine.Clear(r.Context(), user); err != nil && !errors.Is(err, tokens.ErrNotConnected) {
		s.log.Error("disconnect: clear", "user", user, "err", err)
	}
	u, _, err := s.Store.Get(r.Context(), user)
	if err != nil {
		s.log.Error("disconnect: store get", "user", user, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if u.RefreshToken != "" {
		if err := s.Discord.Revoke(r.Context(), u.RefreshToken); err != nil {
			s.log.Error("disconnect: revoke", "user", user, "err", err)
		}
	}
	if err := s.Store.Delete(r.Context(), user); err != nil {
		s.log.Error("disconnect: delete", "user", user, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

func (s *server) logout(w http.ResponseWriter, r *http.Request, _ string) {
	s.setCookie(w, sessionCookie, "", -1)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
