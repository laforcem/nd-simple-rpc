package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/laforcem/nd-simple-rpc/internal/discord"
	"github.com/laforcem/nd-simple-rpc/internal/store"
	"github.com/laforcem/nd-simple-rpc/internal/tokens"
)

type fakeDiscord struct {
	mu       sync.Mutex
	exchange discord.TokenSet
	user     discord.User
	revoked  []string
	events   *[]string
	lastCode string
}

func (f *fakeDiscord) AuthorizeURL(state, ch string) string {
	return "https://discord.test/authorize?state=" + url.QueryEscape(state) +
		"&code_challenge=" + url.QueryEscape(ch) + "&code_challenge_method=S256"
}
func (f *fakeDiscord) ExchangeCode(_ context.Context, code, verifier string) (discord.TokenSet, error) {
	f.lastCode = code
	if verifier == "" {
		return discord.TokenSet{}, errors.New("no verifier")
	}
	return f.exchange, nil
}
func (f *fakeDiscord) CurrentUser(context.Context, string) (discord.User, error) { return f.user, nil }
func (f *fakeDiscord) Revoke(_ context.Context, tok string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = append(f.revoked, tok)
	if f.events != nil {
		*f.events = append(*f.events, "revoke:"+tok)
	}
	return nil
}

type fakeEngine struct {
	cleared       []string
	err           error
	events        *[]string
	st            *store.Store
	pausedAtClear []bool
}

func (f *fakeEngine) Clear(ctx context.Context, u string) error {
	f.cleared = append(f.cleared, u)
	if f.events != nil {
		*f.events = append(*f.events, "clear")
	}
	if f.st != nil {
		usr, _, _ := f.st.Get(ctx, u)
		f.pausedAtClear = append(f.pausedAtClear, usr.Paused)
	}
	return f.err
}

type env struct {
	h      http.Handler
	st     *store.Store
	dc     *fakeDiscord
	eng    *fakeEngine
	nd     *httptest.Server
	logs   *bytes.Buffer
	dbPath string
	events []string
	ndHits atomic.Int32
}

func newEnv(t *testing.T, ndUser string) *env {
	t.Helper()
	e := &env{logs: &bytes.Buffer{}}
	e.nd = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Username, Password string }
		_ = decodeJSON(r, &body)
		e.ndHits.Add(1)
		if body.Password == "bad" {
			time.Sleep(20 * time.Millisecond)
		}
		if body.Password != "good" {
			w.WriteHeader(401)
			return
		}
		w.Write([]byte(`{"username":"` + ndUser + `"}`))
	}))
	t.Cleanup(e.nd.Close)
	e.dbPath = filepath.Join(t.TempDir(), "db.sqlite")
	st, err := store.Open(e.dbPath, bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e.st = st
	e.dc = &fakeDiscord{
		exchange: discord.TokenSet{AccessToken: "at", RefreshToken: "rt", ExpiresAt: time.Now().Add(time.Hour)},
		user:     discord.User{ID: "d1", Username: "discordname"},
		events:   &e.events,
	}
	e.eng = &fakeEngine{events: &e.events, st: st}
	h, err := NewPublic(Deps{
		Store: st, Discord: e.dc, Engine: e.eng,
		NavidromeURL: e.nd.URL, PublicURL: "https://presence.example",
		SessionKey: bytes.Repeat([]byte{9}, 32),
		Log:        slog.New(slog.NewTextHandler(e.logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	e.h = h
	return e
}

func (e *env) do(method, path string, form url.Values, cookies []*http.Cookie, hdr map[string]string) *httptest.ResponseRecorder {
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	r := httptest.NewRequest(method, path, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func cookie(w *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func (e *env) login(t *testing.T, typed string) *http.Cookie {
	t.Helper()
	w := e.do("POST", "/login", url.Values{"username": {typed}, "password": {"good"}}, nil, nil)
	if w.Code != 303 && w.Code != 302 {
		t.Fatalf("login status %d: %s", w.Code, w.Body)
	}
	c := cookie(w, "__Host-nsr_session")
	if c == nil {
		t.Fatal("no session cookie")
	}
	return c
}

// csrfFor extracts the CSRF token from the settings page.
func (e *env) csrfFor(t *testing.T, sess *http.Cookie) string {
	t.Helper()
	w := e.do("GET", "/settings", nil, []*http.Cookie{sess}, nil)
	body := w.Body.String()
	i := strings.Index(body, `name="csrf" value="`)
	if i < 0 {
		t.Fatalf("no csrf field in: %s", body)
	}
	rest := body[i+len(`name="csrf" value="`):]
	return rest[:strings.Index(rest, `"`)]
}

func TestLoginCanonicalUsername(t *testing.T) {
	e := newEnv(t, "Malc")
	sess := e.login(t, "malc")
	if !sess.Secure || !sess.HttpOnly || sess.SameSite != http.SameSiteLaxMode || sess.Path != "/" {
		t.Errorf("cookie attrs: %+v", sess)
	}
	w := e.do("GET", "/settings", nil, []*http.Cookie{sess}, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Malc") {
		t.Fatalf("settings: %d %s", w.Code, w.Body)
	}
	// connect + callback stores under Malc
	cw := e.do("GET", "/connect", nil, []*http.Cookie{sess}, nil)
	oauth := cookie(cw, "__Host-nsr_oauth")
	state := url.QueryEscape("")
	loc, _ := url.Parse(cw.Header().Get("Location"))
	state = loc.Query().Get("state")
	cb := e.do("GET", "/callback?code=abc&state="+url.QueryEscape(state), nil, []*http.Cookie{sess, oauth}, nil)
	if cb.Code != 302 && cb.Code != 303 {
		t.Fatalf("callback %d %s", cb.Code, cb.Body)
	}
	if _, ok, _ := e.st.Get(context.Background(), "Malc"); !ok {
		t.Fatal("row not stored under canonical name")
	}
	if _, ok, _ := e.st.Get(context.Background(), "malc"); ok {
		t.Fatal("row stored under typed name")
	}
}

func TestLoginBadPassword(t *testing.T) {
	e := newEnv(t, "Malc")
	w := e.do("POST", "/login", url.Values{"username": {"malc"}, "password": {"bad"}}, nil, nil)
	if w.Code != 200 && w.Code != 401 {
		t.Fatalf("status %d", w.Code)
	}
	if cookie(w, "__Host-nsr_session") != nil {
		t.Fatal("cookie set")
	}
	if !strings.Contains(w.Body.String(), "Invalid") {
		t.Fatalf("no error shown: %s", w.Body)
	}
}

func TestLoginRateLimited(t *testing.T) {
	e := newEnv(t, "Malc")
	hdr := map[string]string{"X-Forwarded-For": "1.2.3.4, 10.0.0.1"}
	for i := 0; i < 5; i++ {
		w := e.do("POST", "/login", url.Values{"username": {"m"}, "password": {"bad"}}, nil, hdr)
		if w.Code == 429 {
			t.Fatalf("limited early at %d", i)
		}
	}
	w := e.do("POST", "/login", url.Values{"username": {"m"}, "password": {"good"}}, nil, hdr)
	if w.Code != 429 {
		t.Fatalf("status %d", w.Code)
	}
	// a different client IP is unaffected
	w = e.do("POST", "/login", url.Values{"username": {"m"}, "password": {"good"}}, nil, map[string]string{"X-Forwarded-For": "5.6.7.8"})
	if w.Code == 429 {
		t.Fatal("other IP limited")
	}
}

func TestLoginNavidromeDownIsErrorPage(t *testing.T) {
	e := newEnv(t, "Malc")
	e.nd.Close()
	w := e.do("POST", "/login", url.Values{"username": {"m"}, "password": {"good"}}, nil, nil)
	if w.Code != 502 || cookie(w, "__Host-nsr_session") != nil {
		t.Fatalf("status %d", w.Code)
	}
}

func TestPasswordNeverStored(t *testing.T) {
	e := newEnv(t, "Malc")
	pw := "good"
	sess := e.login(t, "malc")
	e.do("GET", "/connect", nil, []*http.Cookie{sess}, nil)
	e.do("POST", "/login", url.Values{"username": {"malc"}, "password": {"hunter2-secret"}}, nil, nil)
	if strings.Contains(e.logs.String(), "hunter2-secret") || strings.Contains(e.logs.String(), "password="+pw) {
		t.Fatal("password in logs")
	}
	files, _ := filepath.Glob(e.dbPath + "*")
	for _, f := range files {
		b, _ := os.ReadFile(f)
		if bytes.Contains(b, []byte("hunter2-secret")) || bytes.Contains(b, []byte(pw+"\"")) {
			t.Fatalf("password in %s", f)
		}
	}
}

func TestSettingsRequiresLogin(t *testing.T) {
	e := newEnv(t, "Malc")
	w := e.do("GET", "/settings", nil, nil, nil)
	if w.Code != 302 && w.Code != 303 || w.Header().Get("Location") != "/" {
		t.Fatalf("%d %q", w.Code, w.Header().Get("Location"))
	}
}

func TestTamperedSessionRejected(t *testing.T) {
	e := newEnv(t, "Malc")
	sess := e.login(t, "malc")
	bad := &http.Cookie{Name: sess.Name, Value: sess.Value + "x"}
	w := e.do("GET", "/settings", nil, []*http.Cookie{bad}, nil)
	if w.Code != 302 && w.Code != 303 {
		t.Fatalf("%d", w.Code)
	}
}

func TestConnectRedirect(t *testing.T) {
	e := newEnv(t, "Malc")
	sess := e.login(t, "malc")
	w := e.do("GET", "/connect", nil, []*http.Cookie{sess}, nil)
	if w.Code != 302 {
		t.Fatalf("%d", w.Code)
	}
	loc, _ := url.Parse(w.Header().Get("Location"))
	if loc.Query().Get("state") == "" || loc.Query().Get("code_challenge") == "" || loc.Query().Get("code_challenge_method") != "S256" {
		t.Fatalf("loc %s", loc)
	}
	c := cookie(w, "__Host-nsr_oauth")
	if c == nil || !c.Secure || !c.HttpOnly || c.MaxAge != 600 {
		t.Fatalf("oauth cookie %+v", c)
	}
}

func TestConnectPageWarnsScope(t *testing.T) {
	e := newEnv(t, "Malc")
	sess := e.login(t, "malc")
	w := e.do("GET", "/settings", nil, []*http.Cookie{sess}, nil)
	b := w.Body.String()
	if !strings.Contains(b, "friends-list") || !strings.Contains(b, "Connect Discord") || !strings.Contains(b, "Not connected") {
		t.Fatalf("body: %s", b)
	}
}

func TestCallbackStateMismatch(t *testing.T) {
	e := newEnv(t, "Malc")
	sess := e.login(t, "malc")
	cw := e.do("GET", "/connect", nil, []*http.Cookie{sess}, nil)
	oauth := cookie(cw, "__Host-nsr_oauth")
	w := e.do("GET", "/callback?code=abc&state=wrong", nil, []*http.Cookie{sess, oauth}, nil)
	if w.Code != 400 {
		t.Fatalf("%d", w.Code)
	}
	if _, ok, _ := e.st.Get(context.Background(), "Malc"); ok {
		t.Fatal("stored")
	}
	// no oauth cookie at all
	w = e.do("GET", "/callback?code=abc&state=wrong", nil, []*http.Cookie{sess}, nil)
	if w.Code != 400 {
		t.Fatalf("%d", w.Code)
	}
}

func connected(t *testing.T, e *env) (*http.Cookie, string) {
	t.Helper()
	sess := e.login(t, "malc")
	cw := e.do("GET", "/connect", nil, []*http.Cookie{sess}, nil)
	loc, _ := url.Parse(cw.Header().Get("Location"))
	e.do("GET", "/callback?code=abc&state="+url.QueryEscape(loc.Query().Get("state")), nil,
		[]*http.Cookie{sess, cookie(cw, "__Host-nsr_oauth")}, nil)
	return sess, e.csrfFor(t, sess)
}

func TestCallbackStoresTokens(t *testing.T) {
	e := newEnv(t, "Malc")
	// pre-existing flagged row is replaced wholesale
	e.st.Put(context.Background(), store.User{Username: "Malc", NeedsReconnect: true, Paused: true})
	connected(t, e)
	u, ok, _ := e.st.Get(context.Background(), "Malc")
	if !ok || u.DiscordUserID != "d1" || u.DiscordUsername != "discordname" ||
		u.RefreshToken != "rt" || u.AccessToken != "at" || u.NeedsReconnect || u.Paused || u.SessionToken != "" {
		t.Fatalf("%+v", u)
	}
	if u.AccessExpiresAt.IsZero() {
		t.Error("no expiry")
	}
}

func TestPauseClearsAndFlags(t *testing.T) {
	e := newEnv(t, "Malc")
	sess, csrf := connected(t, e)
	w := e.do("POST", "/pause", url.Values{"csrf": {csrf}}, []*http.Cookie{sess}, nil)
	if w.Code != 303 && w.Code != 302 {
		t.Fatalf("%d", w.Code)
	}
	u, _, _ := e.st.Get(context.Background(), "Malc")
	if !u.Paused || len(e.eng.cleared) != 1 || e.eng.cleared[0] != "Malc" {
		t.Fatalf("%+v %v", u, e.eng.cleared)
	}
	if len(e.eng.pausedAtClear) != 1 || !e.eng.pausedAtClear[0] {
		t.Fatal("Paused must be set before Clear")
	}
}

func TestPauseNotConnectedClearNonFatal(t *testing.T) {
	e := newEnv(t, "Malc")
	sess, csrf := connected(t, e)
	e.eng.err = tokens.ErrNotConnected
	w := e.do("POST", "/pause", url.Values{"csrf": {csrf}}, []*http.Cookie{sess}, nil)
	if w.Code != 303 && w.Code != 302 {
		t.Fatalf("%d", w.Code)
	}
}

func TestResume(t *testing.T) {
	e := newEnv(t, "Malc")
	sess, csrf := connected(t, e)
	e.do("POST", "/pause", url.Values{"csrf": {csrf}}, []*http.Cookie{sess}, nil)
	e.eng.cleared = nil
	w := e.do("POST", "/resume", url.Values{"csrf": {csrf}}, []*http.Cookie{sess}, nil)
	if w.Code != 303 && w.Code != 302 {
		t.Fatalf("%d", w.Code)
	}
	u, _, _ := e.st.Get(context.Background(), "Malc")
	if u.Paused || len(e.eng.cleared) != 0 {
		t.Fatalf("%+v %v", u, e.eng.cleared)
	}
}

func TestDisconnect(t *testing.T) {
	e := newEnv(t, "Malc")
	sess, csrf := connected(t, e)
	e.events = nil
	w := e.do("POST", "/disconnect", url.Values{"csrf": {csrf}}, []*http.Cookie{sess}, nil)
	if w.Code != 303 && w.Code != 302 {
		t.Fatalf("%d", w.Code)
	}
	if strings.Join(e.events, ",") != "clear,revoke:rt" {
		t.Fatalf("events %v", e.events)
	}
	if _, ok, _ := e.st.Get(context.Background(), "Malc"); ok {
		t.Fatal("row remains")
	}
}

func TestDisconnectProceedsWhenClearFails(t *testing.T) {
	e := newEnv(t, "Malc")
	sess, csrf := connected(t, e)
	e.eng.err = errors.New("boom")
	e.do("POST", "/disconnect", url.Values{"csrf": {csrf}}, []*http.Cookie{sess}, nil)
	if _, ok, _ := e.st.Get(context.Background(), "Malc"); ok {
		t.Fatal("row remains")
	}
	if len(e.dc.revoked) != 1 {
		t.Fatal("not revoked")
	}
}

func TestPostWithoutCSRF(t *testing.T) {
	e := newEnv(t, "Malc")
	sess, _ := connected(t, e)
	for _, p := range []string{"/pause", "/resume", "/disconnect", "/logout"} {
		w := e.do("POST", p, url.Values{}, []*http.Cookie{sess}, nil)
		if w.Code != 403 {
			t.Errorf("%s: %d", p, w.Code)
		}
		w = e.do("POST", p, url.Values{"csrf": {"nope"}}, []*http.Cookie{sess}, nil)
		if w.Code != 403 {
			t.Errorf("%s bad token: %d", p, w.Code)
		}
	}
	if len(e.eng.cleared) != 0 {
		t.Fatal("acted without csrf")
	}
}

func TestLogout(t *testing.T) {
	e := newEnv(t, "Malc")
	sess, csrf := connected(t, e)
	w := e.do("POST", "/logout", url.Values{"csrf": {csrf}}, []*http.Cookie{sess}, nil)
	c := cookie(w, "__Host-nsr_session")
	if c == nil || c.MaxAge >= 0 {
		t.Fatalf("%+v", c)
	}
}

func TestSettingsShowsReconnect(t *testing.T) {
	e := newEnv(t, "Malc")
	e.st.Put(context.Background(), store.User{Username: "Malc", DiscordUserID: "d1", DiscordUsername: "x", NeedsReconnect: true})
	sess := e.login(t, "malc")
	w := e.do("GET", "/settings", nil, []*http.Cookie{sess}, nil)
	if !strings.Contains(strings.ToLower(w.Body.String()), "reconnect") {
		t.Fatalf("%s", w.Body)
	}
}

func TestSettingsConnectedAndPausedLabels(t *testing.T) {
	e := newEnv(t, "Malc")
	sess, csrf := connected(t, e)
	b := e.do("GET", "/settings", nil, []*http.Cookie{sess}, nil).Body.String()
	if !strings.Contains(b, "Connected as") || !strings.Contains(b, "discordname") || !strings.Contains(b, ">Pause<") || !strings.Contains(b, ">Disconnect<") {
		t.Fatalf("%s", b)
	}
	e.do("POST", "/pause", url.Values{"csrf": {csrf}}, []*http.Cookie{sess}, nil)
	b = e.do("GET", "/settings", nil, []*http.Cookie{sess}, nil).Body.String()
	if !strings.Contains(b, ">Resume<") {
		t.Fatalf("%s", b)
	}
}

func decodeJSON(r *http.Request, v any) error { return json.NewDecoder(r.Body).Decode(v) }

func TestNewPublicRejectsShortKey(t *testing.T) {
	h, err := NewPublic(Deps{SessionKey: make([]byte, 31)})
	if err == nil || h != nil {
		t.Fatalf("got %v, %v", h, err)
	}
}

func TestLoginCSRFHeaders(t *testing.T) {
	e := newEnv(t, "Malc")
	form := url.Values{"username": {"malc"}, "password": {"good"}}
	cases := []struct {
		name string
		hdr  map[string]string
		want int
	}{
		{"cross-site", map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
		{"foreign origin", map[string]string{"Origin": "https://evil.example"}, 403},
		{"null origin", map[string]string{"Origin": "null"}, 403},
		{"same origin", map[string]string{"Origin": "https://presence.example", "Sec-Fetch-Site": "same-origin"}, 303},
		{"no headers", nil, 303},
	}
	for _, c := range cases {
		w := e.do("POST", "/login", form, nil, c.hdr)
		if w.Code != c.want {
			t.Errorf("%s: %d want %d", c.name, w.Code, c.want)
		}
		if c.want == 403 && cookie(w, "__Host-nsr_session") != nil {
			t.Errorf("%s: cookie set", c.name)
		}
	}
	// also enforced on authenticated POSTs
	sess, csrf := connected(t, e)
	w := e.do("POST", "/pause", url.Values{"csrf": {csrf}}, []*http.Cookie{sess}, map[string]string{"Sec-Fetch-Site": "cross-site"})
	if w.Code != 403 {
		t.Errorf("pause cross-site: %d", w.Code)
	}
}

func TestCallbackRejectsDifferentSessionUser(t *testing.T) {
	e := newEnv(t, "Alice")
	sessA := e.login(t, "alice")
	cw := e.do("GET", "/connect", nil, []*http.Cookie{sessA}, nil)
	loc, _ := url.Parse(cw.Header().Get("Location"))
	// switch to a different user's session
	e.nd.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"username":"Bob"}`)) })
	sessB := e.login(t, "bob")
	w := e.do("GET", "/callback?code=abc&state="+url.QueryEscape(loc.Query().Get("state")), nil,
		[]*http.Cookie{sessB, cookie(cw, "__Host-nsr_oauth")}, nil)
	if w.Code != 400 {
		t.Fatalf("%d", w.Code)
	}
	for _, n := range []string{"Alice", "Bob"} {
		if _, ok, _ := e.st.Get(context.Background(), n); ok {
			t.Fatalf("%s stored", n)
		}
	}
}

func TestLoginReservesSlotBeforeNavidrome(t *testing.T) {
	e := newEnv(t, "Malc")
	hdr := map[string]string{"X-Forwarded-For": "9.9.9.9"}
	var wg sync.WaitGroup
	var limited atomic.Int32
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := e.do("POST", "/login", url.Values{"username": {"m"}, "password": {"bad"}}, nil, hdr)
			if w.Code == 429 {
				limited.Add(1)
			}
		}()
	}
	wg.Wait()
	if n := e.ndHits.Load(); n > 5 {
		t.Fatalf("%d requests reached Navidrome", n)
	}
	if limited.Load() != 5 {
		t.Fatalf("limited = %d", limited.Load())
	}
}

func TestSuccessfulLoginReleasesSlot(t *testing.T) {
	e := newEnv(t, "Malc")
	hdr := map[string]string{"X-Forwarded-For": "8.8.8.8"}
	for i := 0; i < 20; i++ {
		if w := e.do("POST", "/login", url.Values{"username": {"m"}, "password": {"good"}}, nil, hdr); w.Code == 429 {
			t.Fatalf("limited at %d", i)
		}
	}
}
