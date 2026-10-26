// Package api serves the internal endpoint the Navidrome plugin posts to.
package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/laforcem/nd-simple-rpc/internal/event"
)

const maxBody = 64 << 10

// Engine receives decoded playback events.
type Engine interface {
	HandlePlayback(event.Playback)
}

// NewInternal returns the handler for the internal port: POST
// /internal/v1/playback (bearer secret) and GET /healthz.
func NewInternal(secret string, e Engine) http.Handler {
	want := sha256.Sum256([]byte(secret))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("POST /internal/v1/playback", func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		got := sha256.Sum256([]byte(tok))
		if !ok || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		var p event.Playback
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil || p.Username == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if p.Timestamp == 0 {
			p.Timestamp = time.Now().Unix()
		}
		e.HandlePlayback(p)
		w.WriteHeader(http.StatusAccepted)
	})
	return mux
}
