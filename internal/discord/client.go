// Package discord is a minimal client for Discord's OAuth2 token endpoints and
// the headless-session presence endpoints.
package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/laforcem/nd-simple-rpc/internal/activity"
)

// Version is reported in the User-Agent; the main binary may set it.
var Version = "dev"

const (
	defaultBaseURL  = "https://discord.com/api/v10"
	authorizeURL    = "https://discord.com/oauth2/authorize"
	scope           = "openid sdk.social_layer_presence"
	requestTimeout  = 10 * time.Second
	defaultRetry    = 2 * time.Second
	maxRetry        = 10 * time.Minute
	maxResponseBody = 1 << 20
)

type Client struct {
	BaseURL      string // defaults to https://discord.com/api/v10
	ClientID     string
	ClientSecret string
	RedirectURI  string
	HTTP         *http.Client     // nil uses a client with a 10 s timeout
	Now          func() time.Time // nil uses time.Now
}

type TokenSet struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) base() string {
	if c.BaseURL == "" {
		return defaultBaseURL
	}
	return strings.TrimRight(c.BaseURL, "/")
}

// AuthorizeURL returns the consent-screen URL for an authorization-code flow with PKCE.
func (c *Client) AuthorizeURL(state, codeChallenge string) string {
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {c.ClientID},
		"scope":                 {scope},
		"redirect_uri":          {c.RedirectURI},
		"state":                 {state},
		"code_challenge":        {codeChallenge},
		"code_challenge_method": {"S256"},
	}
	return authorizeURL + "?" + q.Encode()
}

func (c *Client) ExchangeCode(ctx context.Context, code, verifier string) (TokenSet, error) {
	return c.token(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {c.RedirectURI},
	})
}

func (c *Client) Refresh(ctx context.Context, refreshToken string) (TokenSet, error) {
	return c.token(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	})
}

func (c *Client) Revoke(ctx context.Context, token string) error {
	resp, err := c.do(ctx, "revoke", "POST", "/oauth2/token/revoke", "",
		"application/x-www-form-urlencoded", true,
		[]byte(url.Values{"token": {token}, "token_type_hint": {"refresh_token"}}.Encode()))
	if err != nil {
		return err
	}
	return c.check("revoke", resp, false)
}

func (c *Client) CurrentUser(ctx context.Context, accessToken string) (User, error) {
	resp, err := c.do(ctx, "current user", "GET", "/users/@me", accessToken, "", false, nil)
	if err != nil {
		return User{}, err
	}
	if err := c.check("current user", resp, false); err != nil {
		return User{}, err
	}
	var u User
	if err := json.Unmarshal(resp.body, &u); err != nil {
		return User{}, fmt.Errorf("discord: current user: decode response: %w", err)
	}
	return u, nil
}

// PutSession creates (empty sessionToken) or updates a headless session and
// returns its token. If an update response carries no token, the input is returned.
func (c *Client) PutSession(ctx context.Context, accessToken, sessionToken string, a activity.Activity) (string, error) {
	body := struct {
		Activities []activity.Activity `json:"activities"`
		Token      string              `json:"token,omitempty"`
	}{Activities: []activity.Activity{a}, Token: sessionToken}
	b, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	resp, err := c.do(ctx, "put session", "POST", "/users/@me/headless-sessions", accessToken, "application/json", false, b)
	if err != nil {
		return "", err
	}
	if err := c.check("put session", resp, true); err != nil {
		return "", err
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(resp.body, &out); err != nil {
		return "", fmt.Errorf("discord: put session: decode response: %w", err)
	}
	if out.Token == "" {
		out.Token = sessionToken
	}
	if out.Token == "" {
		return "", fmt.Errorf("discord: put session: response carried no token")
	}
	return out.Token, nil
}

func (c *Client) DeleteSession(ctx context.Context, accessToken, sessionToken string) error {
	b, err := json.Marshal(map[string]string{"token": sessionToken})
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, "delete session", "POST", "/users/@me/headless-sessions/delete", accessToken, "application/json", false, b)
	if err != nil {
		return err
	}
	return c.check("delete session", resp, true)
}

func (c *Client) token(ctx context.Context, form url.Values) (TokenSet, error) {
	resp, err := c.do(ctx, "token", "POST", "/oauth2/token", "",
		"application/x-www-form-urlencoded", true, []byte(form.Encode()))
	if err != nil {
		return TokenSet{}, err
	}
	if resp.status == http.StatusBadRequest {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(resp.body, &e) == nil && e.Error == "invalid_grant" {
			return TokenSet{}, ErrInvalidGrant
		}
	}
	if err := c.check("token", resp, false); err != nil {
		return TokenSet{}, err
	}
	var t struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(resp.body, &t); err != nil {
		return TokenSet{}, fmt.Errorf("discord: token: decode response: %w", err)
	}
	if t.AccessToken == "" || t.ExpiresIn <= 0 {
		return TokenSet{}, fmt.Errorf("discord: token: response missing access_token or a positive expires_in")
	}
	return TokenSet{
		AccessToken:  t.AccessToken,
		RefreshToken: t.RefreshToken,
		ExpiresAt:    c.now().Add(time.Duration(t.ExpiresIn) * time.Second),
	}, nil
}

type response struct {
	status int
	header http.Header
	body   []byte
}

func (c *Client) do(ctx context.Context, op, method, path, bearer, contentType string, basic bool, body []byte) (*response, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.base()+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("discord: %s: build request: %w", op, err)
	}
	req.Header.Set("User-Agent", "DiscordBot (https://github.com/laforcem/nd-simple-rpc, "+Version+")")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if basic {
		req.SetBasicAuth(c.ClientID, c.ClientSecret)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: requestTimeout}
	}
	res, err := hc.Do(req)
	if err != nil {
		// Report only the cause; *url.Error would add the URL, which is harmless but noisy.
		if ue, ok := err.(interface{ Unwrap() error }); ok && ue.Unwrap() != nil {
			err = ue.Unwrap()
		}
		return nil, fmt.Errorf("discord: %s: request failed: %w", op, err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBody))
	if err != nil {
		return nil, fmt.Errorf("discord: %s: read response: %w", op, err)
	}
	return &response{status: res.StatusCode, header: res.Header, body: b}, nil
}

// check maps non-2xx statuses to errors. sessionEndpoint makes a 400 mean ErrSessionGone.
func (c *Client) check(op string, r *response, sessionEndpoint bool) error {
	switch {
	case r.status >= 200 && r.status < 300:
		return nil
	case r.status == http.StatusTooManyRequests:
		return &RateLimitError{RetryAfter: retryAfter(r.header)}
	case r.status == http.StatusUnauthorized:
		return ErrUnauthorized
	case r.status == http.StatusBadRequest && sessionEndpoint:
		return ErrSessionGone
	}
	return &statusError{op: op, status: r.status}
}

func retryAfter(h http.Header) time.Duration {
	for _, k := range []string{"Retry-After", "X-RateLimit-Reset-After"} {
		if v := h.Get(k); v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) && f >= 0 {
				if f >= maxRetry.Seconds() {
					return maxRetry
				}
				return time.Duration(f * float64(time.Second))
			}
		}
	}
	return defaultRetry
}
