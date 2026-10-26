package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

const (
	sessionCookie = "nsr_session"
	oauthCookie   = "nsr_oauth"
	sessionTTL    = 30 * 24 * time.Hour
	oauthTTL      = 10 * time.Minute
)

var b64 = base64.RawURLEncoding

func (s *server) mac(purpose, data string) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(purpose))
	m.Write([]byte{0})
	m.Write([]byte(data))
	return hex.EncodeToString(m.Sum(nil))
}

// sign returns "<payload>.<expiry>.<mac>" where payload is already cookie-safe.
func (s *server) sign(purpose, payload string, ttl time.Duration) string {
	body := payload + "." + strconv.FormatInt(s.now().Add(ttl).Unix(), 10)
	return body + "." + s.mac(purpose, body)
}

// verify returns the payload if the value is correctly signed and unexpired.
func (s *server) verify(purpose, value string) (string, bool) {
	i := strings.LastIndexByte(value, '.')
	if i < 0 {
		return "", false
	}
	body, sig := value[:i], value[i+1:]
	if !hmac.Equal([]byte(sig), []byte(s.mac(purpose, body))) {
		return "", false
	}
	j := strings.LastIndexByte(body, '.')
	if j < 0 {
		return "", false
	}
	exp, err := strconv.ParseInt(body[j+1:], 10, 64)
	if err != nil || !s.now().Before(time.Unix(exp, 0)) {
		return "", false
	}
	return body[:j], true
}

func (s *server) sessionValue(username string) string {
	return s.sign("session", b64.EncodeToString([]byte(username)), sessionTTL)
}

func (s *server) sessionUser(value string) (string, bool) {
	p, ok := s.verify("session", value)
	if !ok {
		return "", false
	}
	b, err := b64.DecodeString(p)
	if err != nil || len(b) == 0 {
		return "", false
	}
	return string(b), true
}

func (s *server) csrf(sessionValue string) string { return s.mac("csrf", sessionValue) }

func (s *server) csrfOK(sessionValue, token string) bool {
	return hmac.Equal([]byte(token), []byte(s.csrf(sessionValue)))
}

func (s *server) oauthValue(state, verifier string) string {
	return s.sign("oauth", state+"~"+verifier, oauthTTL)
}

// state and verifier are base64url, which never contains '~'.
func (s *server) oauthParts(value string) (state, verifier string, ok bool) {
	p, ok := s.verify("oauth", value)
	if !ok {
		return "", "", false
	}
	state, verifier, ok = strings.Cut(p, "~")
	return state, verifier, ok && state != "" && verifier != ""
}
