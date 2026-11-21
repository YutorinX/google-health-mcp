package auth

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func TestLoadConfigFromFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GOOGLE_HEALTH_CLIENT_ID", "")
	os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(`{"installed":{"client_id":"abc.apps.googleusercontent.com","client_secret":"s3cret"}}`), 0o600)
	cfg, err := LoadConfig(dir, DefaultScopes)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClientID != "abc.apps.googleusercontent.com" || cfg.ClientSecret != "s3cret" {
		t.Errorf("cfg = %+v", cfg)
	}
	if cfg.Scopes[2] != "https://www.googleapis.com/auth/googlehealth.sleep.readonly" {
		t.Errorf("scopes = %v", cfg.Scopes)
	}
}

func TestLoadConfigRejectsWebClient(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GOOGLE_HEALTH_CLIENT_ID", "")
	os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(`{"web":{"client_id":"x"}}`), 0o600)
	if _, err := LoadConfig(dir, nil); err == nil || !strings.Contains(err.Error(), "Desktop app") {
		t.Fatalf("err = %v", err)
	}
}

func TestSaveTokenPermissionsAndRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "token.json")
	tok := &oauth2.Token{AccessToken: "a", RefreshToken: "r", Expiry: time.Now().Add(time.Hour)}
	if err := SaveToken(path, tok, "s1 s2"); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("perm = %v", fi.Mode().Perm())
	}
	got, scopes, err := LoadToken(path)
	if err != nil || got.RefreshToken != "r" || scopes != "s1 s2" {
		t.Errorf("got %+v %q %v", got, scopes, err)
	}
	if _, _, err := LoadToken(filepath.Join(t.TempDir(), "missing.json")); !errors.Is(err, ErrNotLoggedIn) {
		t.Errorf("err = %v", err)
	}
}

func TestFileTokenSourceRefreshesAndSaves(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("refresh_token") != "r" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "fresh", "token_type": "Bearer", "expires_in": 3600})
	}))
	defer ts.Close()
	cfg := &oauth2.Config{ClientID: "c", Endpoint: oauth2.Endpoint{TokenURL: ts.URL, AuthStyle: oauth2.AuthStyleInParams}}

	path := filepath.Join(t.TempDir(), "token.json")
	SaveToken(path, &oauth2.Token{AccessToken: "old", RefreshToken: "r", Expiry: time.Now().Add(-time.Minute)}, "scopes")

	src := NewFileTokenSource(cfg, path)
	tok, err := src.Token()
	if err != nil || tok.AccessToken != "fresh" {
		t.Fatalf("tok = %+v, %v", tok, err)
	}
	saved, scopes, _ := LoadToken(path)
	if saved.AccessToken != "fresh" || saved.RefreshToken != "r" || scopes != "scopes" {
		t.Errorf("saved = %+v %q", saved, scopes)
	}

	// A revoked/expired refresh token gives a re-login hint.
	SaveToken(path, &oauth2.Token{AccessToken: "old", RefreshToken: "bad", Expiry: time.Now().Add(-time.Minute)}, "")
	os.Chtimes(path, time.Now().Add(time.Second), time.Now().Add(time.Second))
	if _, err := src.Token(); err == nil || !strings.Contains(err.Error(), "login") {
		t.Errorf("err = %v", err)
	}
}

func TestNotLoggedIn(t *testing.T) {
	src := NewFileTokenSource(&oauth2.Config{}, filepath.Join(t.TempDir(), "token.json"))
	if _, err := src.Token(); !errors.Is(err, ErrNotLoggedIn) {
		t.Errorf("err = %v", err)
	}
}

func TestMissingScopes(t *testing.T) {
	req := FullScopes([]string{"sleep.readonly", "ecg.readonly"})
	got := MissingScopes(req, ScopePrefix+"sleep.readonly")
	if len(got) != 1 || got[0] != "ecg.readonly" {
		t.Errorf("got %v", got)
	}
}
