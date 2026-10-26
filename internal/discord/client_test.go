package discord

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/laforcem/nd-simple-rpc/internal/activity"
)

var fixedNow = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func newTest(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &Client{
		BaseURL: srv.URL, ClientID: "cid", ClientSecret: "csecret",
		RedirectURI: "https://example.test/cb", HTTP: srv.Client(),
		Now: func() time.Time { return fixedNow },
	}
}

func checkBasic(t *testing.T, r *http.Request) {
	t.Helper()
	u, p, ok := r.BasicAuth()
	if !ok || u != "cid" || p != "csecret" {
		t.Errorf("basic auth = %q %q %v", u, p, ok)
	}
}

func formOf(t *testing.T, r *http.Request) url.Values {
	t.Helper()
	if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
		t.Errorf("content-type = %q", ct)
	}
	b, _ := io.ReadAll(r.Body)
	v, err := url.ParseQuery(string(b))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestAuthorizeURL(t *testing.T) {
	c := &Client{ClientID: "cid", RedirectURI: "https://example.test/cb"}
	u, err := url.Parse(c.AuthorizeURL("st", "chal"))
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "https" || u.Host != "discord.com" || u.Path != "/oauth2/authorize" {
		t.Errorf("url = %s", u)
	}
	q := u.Query()
	want := map[string]string{
		"response_type": "code", "client_id": "cid", "scope": "openid sdk.social_layer_presence",
		"redirect_uri": "https://example.test/cb", "state": "st",
		"code_challenge": "chal", "code_challenge_method": "S256",
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, q.Get(k), v)
		}
	}
}

func TestExchangeCode(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/oauth2/token" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		checkBasic(t, r)
		if ua := r.Header.Get("User-Agent"); !strings.HasPrefix(ua, "DiscordBot (https://github.com/laforcem/nd-simple-rpc, ") {
			t.Errorf("UA = %q", ua)
		}
		f := formOf(t, r)
		for k, v := range map[string]string{
			"grant_type": "authorization_code", "code": "thecode",
			"code_verifier": "ver", "redirect_uri": "https://example.test/cb",
		} {
			if f.Get(k) != v {
				t.Errorf("%s = %q, want %q", k, f.Get(k), v)
			}
		}
		w.Write([]byte(`{"access_token":"at","refresh_token":"rt","expires_in":604800}`))
	})
	ts, err := c.ExchangeCode(context.Background(), "thecode", "ver")
	if err != nil {
		t.Fatal(err)
	}
	if ts.AccessToken != "at" || ts.RefreshToken != "rt" || !ts.ExpiresAt.Equal(fixedNow.Add(604800*time.Second)) {
		t.Errorf("ts = %+v", ts)
	}
}

func TestRefresh(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		checkBasic(t, r)
		f := formOf(t, r)
		if f.Get("grant_type") != "refresh_token" || f.Get("refresh_token") != "old" {
			t.Errorf("form = %v", f)
		}
		w.Write([]byte(`{"access_token":"at2","refresh_token":"rt2","expires_in":60}`))
	})
	ts, err := c.Refresh(context.Background(), "old")
	if err != nil || ts.RefreshToken != "rt2" || !ts.ExpiresAt.Equal(fixedNow.Add(time.Minute)) {
		t.Errorf("ts=%+v err=%v", ts, err)
	}
}

func TestRefreshInvalidGrant(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"invalid_grant","error_description":"Invalid refresh token"}`))
	})
	_, err := c.Refresh(context.Background(), "old")
	if !errors.Is(err, ErrInvalidGrant) {
		t.Errorf("err = %v", err)
	}
}

func TestPutSessionCreate(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/users/@me/headless-sessions" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer at" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if _, ok := body["token"]; ok {
			t.Error("create body has token")
		}
		var acts []activity.Activity
		if err := json.Unmarshal(body["activities"], &acts); err != nil || len(acts) != 1 || acts[0].Name != "Artist" || acts[0].Type != 2 {
			t.Errorf("activities = %s err=%v", body["activities"], err)
		}
		w.Write([]byte(`{"token":"s1"}`))
	})
	tok, err := c.PutSession(context.Background(), "at", "", activity.Activity{Type: 2, Name: "Artist"})
	if err != nil || tok != "s1" {
		t.Errorf("tok=%q err=%v", tok, err)
	}
}

func TestPutSessionUpdate(t *testing.T) {
	for _, resp := range []string{`{"token":"s2"}`, `{}`} {
		c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Token string `json:"token"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if body.Token != "s1" {
				t.Errorf("token = %q", body.Token)
			}
			w.Write([]byte(resp))
		})
		tok, err := c.PutSession(context.Background(), "at", "s1", activity.Activity{Type: 2})
		want := "s1"
		if resp != `{}` {
			want = "s2"
		}
		if err != nil || tok != want {
			t.Errorf("resp %s: tok=%q err=%v", resp, tok, err)
		}
	}
}

func TestPutSession429(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3.5")
		w.WriteHeader(429)
	})
	_, err := c.PutSession(context.Background(), "at", "", activity.Activity{})
	var rl *RateLimitError
	if !errors.As(err, &rl) || rl.RetryAfter != 3500*time.Millisecond {
		t.Errorf("err = %v", err)
	}
}

func TestRateLimitFallbacks(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Reset-After", "1.25")
		w.WriteHeader(429)
	})
	_, err := c.PutSession(context.Background(), "at", "", activity.Activity{})
	var rl *RateLimitError
	if !errors.As(err, &rl) || rl.RetryAfter != 1250*time.Millisecond {
		t.Errorf("reset-after: %v", err)
	}
	c2 := newTest(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(429) })
	_, err = c2.PutSession(context.Background(), "at", "", activity.Activity{})
	if !errors.As(err, &rl) || rl.RetryAfter != 2*time.Second {
		t.Errorf("default: %v", err)
	}
}

func TestPutSession401(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) })
	_, err := c.PutSession(context.Background(), "at", "", activity.Activity{})
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("err = %v", err)
	}
}

func TestPutSession400(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(400) })
	_, err := c.PutSession(context.Background(), "at", "s1", activity.Activity{})
	if !errors.Is(err, ErrSessionGone) {
		t.Errorf("err = %v", err)
	}
}

func TestDeleteSession(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/users/@me/headless-sessions/delete" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer at" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		var body struct {
			Token string `json:"token"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.Token != "s1" {
			t.Errorf("token = %q", body.Token)
		}
		w.WriteHeader(204)
	})
	if err := c.DeleteSession(context.Background(), "at", "s1"); err != nil {
		t.Error(err)
	}
}

func TestDeleteSession400(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(400) })
	err := c.DeleteSession(context.Background(), "at", "s1")
	if !errors.Is(err, ErrSessionGone) {
		t.Errorf("err = %v", err)
	}
}

func TestRevoke(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/oauth2/token/revoke" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		checkBasic(t, r)
		f := formOf(t, r)
		if f.Get("token") != "rt" || f.Get("token_type_hint") != "refresh_token" {
			t.Errorf("form = %v", f)
		}
		w.Write([]byte(`{}`))
	})
	if err := c.Revoke(context.Background(), "rt"); err != nil {
		t.Error(err)
	}
}

func TestCurrentUser(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/users/@me" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer at" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		w.Write([]byte(`{"id":"123","username":"malc","extra":1}`))
	})
	u, err := c.CurrentUser(context.Background(), "at")
	if err != nil || u.ID != "123" || u.Username != "malc" {
		t.Errorf("u=%+v err=%v", u, err)
	}
}

func TestErrorsDoNotContainTokens(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		// A hostile or buggy server that echoes request content.
		b, _ := io.ReadAll(r.Body)
		w.WriteHeader(500)
		w.Write([]byte("echo " + r.Header.Get("Authorization") + string(b)))
	})
	ctx := context.Background()
	_, e1 := c.PutSession(ctx, "ACCESSTOK", "SESSIONTOK", activity.Activity{})
	e2 := c.DeleteSession(ctx, "ACCESSTOK", "SESSIONTOK")
	_, e3 := c.CurrentUser(ctx, "ACCESSTOK")
	_, e4 := c.Refresh(ctx, "REFRESHTOK")
	e5 := c.Revoke(ctx, "REFRESHTOK")
	for i, err := range []error{e1, e2, e3, e4, e5} {
		if err == nil {
			t.Errorf("%d: nil error", i)
			continue
		}
		for _, secret := range []string{"ACCESSTOK", "SESSIONTOK", "REFRESHTOK"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%d: error contains %s: %v", i, secret, err)
			}
		}
	}
}
