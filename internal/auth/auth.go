// Package auth handles Google OAuth for a single local user: a one-time
// browser login (loopback redirect + PKCE) and a token file that is
// refreshed and re-saved automatically.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

const (
	AuthURL     = "https://accounts.google.com/o/oauth2/auth"
	TokenURL    = "https://oauth2.googleapis.com/token"
	ScopePrefix = "https://www.googleapis.com/auth/googlehealth."
)

// DefaultScopes are read-only and cover activity, heart/body metrics,
// sleep, profile and settings. Extra groups (nutrition, ecg, irn,
// location) can be added with -extra-scopes.
var DefaultScopes = []string{
	"activity_and_fitness.readonly",
	"health_metrics_and_measurements.readonly",
	"sleep.readonly",
	"profile.readonly",
	"settings.readonly",
}

// ErrNotLoggedIn is returned when there is no saved token yet.
var ErrNotLoggedIn = errors.New("not logged in to Google Health: run `google-health-mcp login` in a terminal first")

// Dir is where credentials.json and token.json live.
func Dir() (string, error) {
	if d := os.Getenv("GOOGLE_HEALTH_MCP_DIR"); d != "" {
		return d, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "google-health-mcp"), nil
}

// FullScopes expands short scope names ("sleep.readonly") to full URLs.
func FullScopes(short []string) []string {
	out := make([]string, 0, len(short))
	for _, s := range short {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if !strings.HasPrefix(s, "https://") {
			s = ScopePrefix + s
		}
		out = append(out, s)
	}
	return out
}

// LoadConfig builds the OAuth config from GOOGLE_HEALTH_CLIENT_ID /
// GOOGLE_HEALTH_CLIENT_SECRET, or else from the "Desktop app" client JSON
// downloaded from Google Cloud Console (credentials.json in dir).
func LoadConfig(dir string, scopes []string) (*oauth2.Config, error) {
	id, secret := os.Getenv("GOOGLE_HEALTH_CLIENT_ID"), os.Getenv("GOOGLE_HEALTH_CLIENT_SECRET")
	if id == "" {
		path := filepath.Join(dir, "credentials.json")
		data, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("no OAuth client found: save your Desktop-app client JSON as %s or set GOOGLE_HEALTH_CLIENT_ID and GOOGLE_HEALTH_CLIENT_SECRET", path)
			}
			return nil, err
		}
		if id, secret, err = parseClientJSON(data); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	return &oauth2.Config{
		ClientID:     id,
		ClientSecret: secret,
		Endpoint:     oauth2.Endpoint{AuthURL: AuthURL, TokenURL: TokenURL},
		Scopes:       FullScopes(scopes),
	}, nil
}

func parseClientJSON(data []byte) (id, secret string, err error) {
	var f struct {
		Installed *struct {
			ClientID     string `json:"client_id"`
			ClientSecret string `json:"client_secret"`
		} `json:"installed"`
		Web *struct{} `json:"web"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return "", "", fmt.Errorf("not valid JSON: %w", err)
	}
	if f.Installed == nil {
		if f.Web != nil {
			return "", "", errors.New(`this is a "Web application" client; create an OAuth client of type "Desktop app" instead`)
		}
		return "", "", errors.New(`missing "installed" section; download the JSON of a "Desktop app" OAuth client`)
	}
	if f.Installed.ClientID == "" {
		return "", "", errors.New("client_id is empty")
	}
	return f.Installed.ClientID, f.Installed.ClientSecret, nil
}

// stored is the on-disk token format.
type stored struct {
	Token  *oauth2.Token `json:"token"`
	Scopes string        `json:"scopes,omitempty"` // space-separated, as granted
}

func TokenPath(dir string) string { return filepath.Join(dir, "token.json") }

// LoadToken reads the saved token and the scopes that were granted.
func LoadToken(path string) (*oauth2.Token, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, "", ErrNotLoggedIn
		}
		return nil, "", err
	}
	var s stored
	if err := json.Unmarshal(data, &s); err != nil || s.Token == nil {
		return nil, "", fmt.Errorf("%s is corrupt; run `google-health-mcp login` again", path)
	}
	return s.Token, s.Scopes, nil
}

// SaveToken writes the token atomically with owner-only permissions.
func SaveToken(path string, tok *oauth2.Token, scopes string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(stored{Token: tok, Scopes: scopes}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".token-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Login runs the browser consent flow and saves the token. It listens on a
// random 127.0.0.1 port, which Desktop-app OAuth clients allow without
// registering a redirect URI.
func Login(ctx context.Context, cfg *oauth2.Config, tokenPath string, openBrowser bool, out io.Writer) (*oauth2.Token, string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", err
	}
	defer ln.Close()

	c := *cfg
	c.RedirectURL = fmt.Sprintf("http://127.0.0.1:%d/callback", ln.Addr().(*net.TCPAddr).Port)
	state := randomString()
	verifier := oauth2.GenerateVerifier()
	authURL := c.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce, oauth2.S256ChallengeOption(verifier))

	type result struct {
		code string
		err  error
	}
	ch := make(chan result, 1)
	srv := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/callback" {
				http.NotFound(w, r)
				return
			}
			q := r.URL.Query()
			var res result
			switch {
			case q.Get("state") != state:
				res.err = errors.New("state mismatch in OAuth callback")
			case q.Get("error") != "":
				res.err = fmt.Errorf("Google returned an error: %s", q.Get("error"))
			case q.Get("code") == "":
				res.err = errors.New("no authorization code in callback")
			default:
				res.code = q.Get("code")
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if res.err != nil {
				fmt.Fprintf(w, "<h2>Login failed</h2><p>%s</p>", htmlEscape(res.err.Error()))
			} else {
				fmt.Fprint(w, "<h2>Google Health MCP is connected.</h2><p>You can close this tab.</p>")
			}
			select {
			case ch <- res:
			default:
			}
		}),
	}
	go srv.Serve(ln)
	defer srv.Close()

	fmt.Fprintf(out, "Open this URL in your browser to grant access:\n\n%s\n\n", authURL)
	if openBrowser {
		_ = openURL(authURL)
	}
	fmt.Fprintln(out, "Waiting for you to finish in the browser…")

	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	var res result
	select {
	case res = <-ch:
	case <-ctx.Done():
		return nil, "", errors.New("timed out waiting for the browser login")
	}
	if res.err != nil {
		return nil, "", res.err
	}
	tok, err := c.Exchange(ctx, res.code, oauth2.VerifierOption(verifier))
	if err != nil {
		return nil, "", fmt.Errorf("exchanging code for token: %w", err)
	}
	if tok.RefreshToken == "" {
		return nil, "", errors.New("Google didn't return a refresh token; remove the app's access at https://myaccount.google.com/permissions and log in again")
	}
	granted, _ := tok.Extra("scope").(string)
	if err := SaveToken(tokenPath, tok, granted); err != nil {
		return nil, "", err
	}
	return tok, granted, nil
}

// MissingScopes reports requested scopes that are not in the granted list.
func MissingScopes(requested []string, granted string) []string {
	have := map[string]bool{}
	for _, s := range strings.Fields(granted) {
		have[s] = true
	}
	var missing []string
	for _, s := range requested {
		if !have[s] {
			missing = append(missing, strings.TrimPrefix(s, ScopePrefix))
		}
	}
	return missing
}

// FileTokenSource serves tokens from the token file, refreshing them as
// needed and writing refreshed tokens back. It re-reads the file when it
// changes, so `login` can be run while the server is running.
type FileTokenSource struct {
	cfg  *oauth2.Config
	path string

	mu      sync.Mutex
	base    oauth2.TokenSource
	modTime time.Time
	last    string
	scopes  string
}

func NewFileTokenSource(cfg *oauth2.Config, path string) *FileTokenSource {
	return &FileTokenSource{cfg: cfg, path: path}
}

func (s *FileTokenSource) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if fi, err := os.Stat(s.path); err == nil && !fi.ModTime().Equal(s.modTime) {
		s.base = nil // file changed (or first use): reload
	}
	if s.base == nil {
		tok, scopes, err := LoadToken(s.path)
		if err != nil {
			return nil, err
		}
		if fi, err := os.Stat(s.path); err == nil {
			s.modTime = fi.ModTime()
		}
		s.base = s.cfg.TokenSource(context.Background(), tok)
		s.last, s.scopes = tok.AccessToken, scopes
	}
	tok, err := s.base.Token()
	if err != nil {
		var re *oauth2.RetrieveError
		if (errors.As(err, &re) && re.ErrorCode == "invalid_grant") || strings.Contains(err.Error(), "invalid_grant") {
			s.base = nil
			return nil, errors.New("Google rejected the saved login (apps in \"Testing\" mode get refresh tokens that expire after 7 days, and revoking access also does this): run `google-health-mcp login` again")
		}
		return nil, err
	}
	if tok.AccessToken != s.last {
		if err := SaveToken(s.path, tok, s.scopes); err == nil {
			if fi, err := os.Stat(s.path); err == nil {
				s.modTime = fi.ModTime()
			}
		}
		s.last = tok.AccessToken
	}
	return tok, nil
}

func randomString() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

func openURL(u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	return cmd.Start()
}
