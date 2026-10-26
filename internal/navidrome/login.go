// Package navidrome talks to the Navidrome server's public auth endpoint.
package navidrome

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrBadCredentials means Navidrome rejected the username or password.
var ErrBadCredentials = errors.New("navidrome: bad credentials")

// ErrRateLimited means Navidrome refused the attempt with 429. Every
// companion login comes from one address, so this is a shared limit.
var ErrRateLimited = errors.New("navidrome: login rate limited")

var httpClient = &http.Client{Timeout: 10 * time.Second}

// Login verifies a Navidrome username and password and returns the canonical
// username Navidrome stores (which may differ in case from what was typed).
// The password is sent once and never retained.
func Login(ctx context.Context, baseURL, username, password string) (string, error) {
	body, err := json.Marshal(map[string]string{"username": username, "password": password})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(baseURL, "/")+"/auth/login", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		// Do not wrap the request: its error text could include the URL only, but keep it terse.
		return "", errors.New("navidrome: login request failed")
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return "", ErrBadCredentials
	case http.StatusTooManyRequests:
		return "", ErrRateLimited
	default:
		return "", fmt.Errorf("navidrome: login returned status %d", resp.StatusCode)
	}
	var out struct {
		Username string `json:"username"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", errors.New("navidrome: malformed login response")
	}
	if out.Username == "" {
		return "", errors.New("navidrome: login response has no username")
	}
	return out.Username, nil
}
