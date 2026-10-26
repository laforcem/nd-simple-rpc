package navidrome

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLoginSuccessReturnsCanonical(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/auth/login" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		var in map[string]string
		json.NewDecoder(r.Body).Decode(&in)
		if in["username"] != "malc" || in["password"] != "pw" {
			t.Errorf("body = %v", in)
		}
		w.Write([]byte(`{"username":"Malc","token":"x"}`))
	}))
	defer srv.Close()
	got, err := Login(context.Background(), srv.URL+"/", "malc", "pw")
	if err != nil || got != "Malc" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestLoginBadCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
	}))
	defer srv.Close()
	if _, err := Login(context.Background(), srv.URL, "a", "b"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("err = %v", err)
	}
}

func TestLoginOtherStatusIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	_, err := Login(context.Background(), srv.URL, "a", "b")
	if err == nil || errors.Is(err, ErrBadCredentials) {
		t.Fatalf("err = %v", err)
	}
}

func TestLoginEmptyUsernameIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	if _, err := Login(context.Background(), srv.URL, "a", "b"); err == nil {
		t.Fatal("want error")
	}
}
