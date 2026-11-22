// Package remote serves the MCP tools over Streamable HTTP for hosted use by
// any MCP client that supports remote servers with OAuth (Claude, ChatGPT,
// Cursor, VS Code, Codex, Gemini CLI, ...).
//
// The server is its own OAuth 2.1 authorization server toward MCP clients
// (protected resource metadata, authorization server metadata, dynamic
// client registration, PKCE) and delegates sign-in to Google. Because client
// registration is open, every authorization shows a consent page naming the
// client and where it will send the user back, bound to the browser by a
// cookie so the Google sign-in step can't be reached without it. The
// Google refresh token is kept (encrypted) inside the tokens the server
// issues; nothing is stored server-side, so each user's data access is
// carried only by their own client's tokens.
package remote

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"

	"github.com/shmsi/google-health-mcp/internal/health"
	"github.com/shmsi/google-health-mcp/internal/tools"
)

const (
	scopeName       = "health.read" // the one scope this server issues
	accessTokenTTL  = time.Hour
	refreshTokenTTL = 60 * 24 * time.Hour
	codeTTL         = 5 * time.Minute
	pendingTTL      = 15 * time.Minute

	googleAuthURL  = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL = "https://oauth2.googleapis.com/token"
)

// Config configures the hosted server.
type Config struct {
	PublicURL          string   // e.g. https://google-health-mcp-123.europe-north1.run.app
	GoogleClientID     string   // "Web application" OAuth client
	GoogleClientSecret string   //
	AllowedEmails      []string // Google accounts allowed to sign in
	AllowAnyAccount    bool     // public mode: any Google account may sign in; AllowedEmails is ignored
	Key                []byte   // ≥32 random bytes; encrypts all issued tokens
	HealthScopes       []string // full Google Health scope URLs
	// RedirectHosts, if set, restricts client redirect_uris to these hosts
	// (strict mode). If empty, any https redirect, http on loopback, or a
	// private-use app scheme (RFC 8252) is accepted, and the consent page is
	// what protects the user.
	RedirectHosts []string
	Location      *time.Location
	Version       string

	// Overridable for tests.
	HealthBaseURL  string
	GoogleAuthURL  string
	GoogleTokenURL string
	HTTPClient     *http.Client // for calls to Google
	Now            func() time.Time
}

// flowCookie binds an authorization to the browser that saw the consent
// page. SameSite=Lax keeps it off cross-site POSTs (so another site can't
// submit the consent form) but sends it on the top-level redirect back from
// Google.
const flowCookie = "ghm_flow"

// blockedSchemes are never valid redirect targets.
var blockedSchemes = []string{"javascript", "data", "vbscript", "file", "about", "blob", "ftp", "ws", "wss"}

type server struct {
	cfg    Config
	seal   *sealer
	google *oauth2.Config
	cache  *mcp.SchemaCache

	mu      sync.Mutex
	sources map[string]oauth2.TokenSource // by hash of Google refresh token
}

// client is the sealed content of a client_id.
type client struct {
	Redirects []string `json:"r"`
	Secret    bool     `json:"s,omitempty"` // client authenticates with a secret
	Name      string   `json:"n,omitempty"`
}

type pending struct {
	ClientID  string `json:"c"`
	Redirect  string `json:"r"`
	State     string `json:"s,omitempty"`
	Challenge string `json:"ch"`
	Verifier  string `json:"v"` // our PKCE verifier toward Google
	Nonce     string `json:"n"` // must match flowCookie in the browser
}

type grant struct {
	ClientID  string `json:"c"`
	Redirect  string `json:"r,omitempty"`
	Challenge string `json:"ch,omitempty"`
	Email     string `json:"m"`
	GRT       string `json:"g"` // Google refresh token
}

// NewHandler returns the HTTP handler for the hosted server.
func NewHandler(cfg Config) (http.Handler, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Location == nil {
		cfg.Location = time.UTC
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if cfg.GoogleAuthURL == "" {
		cfg.GoogleAuthURL = googleAuthURL
	}
	if cfg.GoogleTokenURL == "" {
		cfg.GoogleTokenURL = googleTokenURL
	}
	cfg.PublicURL = strings.TrimRight(cfg.PublicURL, "/")
	if u, err := url.Parse(cfg.PublicURL); err != nil || u.Host == "" || (u.Scheme != "https" && u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1") {
		return nil, fmt.Errorf("PUBLIC_URL must be an https URL, got %q", cfg.PublicURL)
	}
	if cfg.GoogleClientID == "" || cfg.GoogleClientSecret == "" {
		return nil, errors.New("Google OAuth client id and secret are required")
	}
	if len(cfg.AllowedEmails) == 0 && !cfg.AllowAnyAccount {
		return nil, errors.New("at least one allowed email is required (or allow any account explicitly)")
	}
	if cfg.AllowAnyAccount {
		cfg.AllowedEmails = nil
	}
	for i, e := range cfg.AllowedEmails {
		cfg.AllowedEmails[i] = strings.ToLower(strings.TrimSpace(e))
	}
	sl, err := newSealer(cfg.Key, cfg.Now)
	if err != nil {
		return nil, err
	}
	s := &server{
		cfg:  cfg,
		seal: sl,
		google: &oauth2.Config{
			ClientID:     cfg.GoogleClientID,
			ClientSecret: cfg.GoogleClientSecret,
			Endpoint:     oauth2.Endpoint{AuthURL: cfg.GoogleAuthURL, TokenURL: cfg.GoogleTokenURL, AuthStyle: oauth2.AuthStyleInParams},
			RedirectURL:  cfg.PublicURL + "/oauth/google/callback",
			Scopes:       append([]string{"openid", "email"}, cfg.HealthScopes...),
		},
		cache:   mcp.NewSchemaCache(),
		sources: map[string]oauth2.TokenSource{},
	}

	mux := http.NewServeMux()
	prm := auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
		Resource:               cfg.PublicURL + "/mcp",
		AuthorizationServers:   []string{cfg.PublicURL},
		ScopesSupported:        []string{scopeName},
		BearerMethodsSupported: []string{"header"},
	})
	mux.Handle("GET /.well-known/oauth-protected-resource", prm)
	mux.Handle("GET /.well-known/oauth-protected-resource/mcp", prm)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.asMetadata)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server/mcp", s.asMetadata)
	mux.HandleFunc("GET /.well-known/openid-configuration", s.asMetadata)
	mux.HandleFunc("POST /oauth/register", s.register)
	mux.HandleFunc("GET /oauth/authorize", s.authorize)
	mux.HandleFunc("POST /oauth/authorize", s.consent)
	mux.HandleFunc("GET /oauth/google/callback", s.googleCallback)
	mux.HandleFunc("POST /oauth/token", s.token)

	mcpHandler := mcp.NewStreamableHTTPHandler(s.mcpServer, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	mux.Handle("/mcp", auth.RequireBearerToken(s.verify, &auth.RequireBearerTokenOptions{
		ResourceMetadataURL: cfg.PublicURL + "/.well-known/oauth-protected-resource/mcp",
		ClockSkew:           30 * time.Second,
	})(mcpHandler))

	// Cloud Run's front end swallows some paths ending in "z", so /healthz
	// never reaches the app there. /health is the one to use; /healthz stays
	// for local and other hosts.
	ok := func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") }
	mux.HandleFunc("GET /health", ok)
	mux.HandleFunc("GET /healthz", ok)
	mux.HandleFunc("GET /{$}", s.landing)
	return securityHeaders(mux), nil
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'")
		h.ServeHTTP(w, r)
	})
}

// --- metadata -------------------------------------------------------------

func (s *server) asMetadata(w http.ResponseWriter, r *http.Request) {
	base := s.cfg.PublicURL
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                base,
		"authorization_endpoint":                base + "/oauth/authorize",
		"token_endpoint":                        base + "/oauth/token",
		"registration_endpoint":                 base + "/oauth/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none", "client_secret_post", "client_secret_basic"},
		"scopes_supported":                      []string{scopeName},
	})
}

// --- dynamic client registration (RFC 7591) -------------------------------

func (s *server) register(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RedirectURIs            []string `json:"redirect_uris"`
		ClientName              string   `json:"client_name"`
		TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
		GrantTypes              []string `json:"grant_types"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "request body must be JSON")
		return
	}
	if len(req.RedirectURIs) == 0 || len(req.RedirectURIs) > 10 {
		oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "1-10 redirect_uris are required")
		return
	}
	for _, u := range req.RedirectURIs {
		if err := s.checkRedirect(u); err != nil {
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", err.Error())
			return
		}
	}
	method := req.TokenEndpointAuthMethod
	switch method {
	case "":
		method = "client_secret_basic" // RFC 7591 default
	case "none", "client_secret_post", "client_secret_basic":
	default:
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "unsupported token_endpoint_auth_method")
		return
	}
	c := client{Redirects: req.RedirectURIs, Secret: method != "none", Name: truncate(req.ClientName, 100)}
	sealed, err := s.seal.seal("client", c, 0)
	if err != nil {
		serverError(w, err)
		return
	}
	id := "c." + sealed
	resp := map[string]any{
		"client_id":                  id,
		"client_id_issued_at":        s.cfg.Now().Unix(),
		"redirect_uris":              c.Redirects,
		"client_name":                c.Name,
		"token_endpoint_auth_method": method,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
	}
	if c.Secret {
		resp["client_secret"] = s.seal.mac("client-secret:" + id)
		resp["client_secret_expires_at"] = 0
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (s *server) checkRedirect(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Fragment != "" || strings.Contains(raw, "#") {
		return fmt.Errorf("invalid redirect_uri %q", raw)
	}
	scheme := strings.ToLower(u.Scheme)
	host := u.Hostname()
	switch {
	case scheme == "https":
		if host == "" {
			return fmt.Errorf("invalid redirect_uri %q", raw)
		}
	case scheme == "http":
		if !isLoopback(host) {
			return fmt.Errorf("redirect_uri %q must use https", raw)
		}
	case slices.Contains(blockedSchemes, scheme):
		return fmt.Errorf("redirect_uri scheme %q is not allowed", scheme)
	default:
		// Private-use URI scheme of a native app (RFC 8252 7.1), e.g. cursor://.
	}
	if len(s.cfg.RedirectHosts) > 0 {
		if scheme != "https" && scheme != "http" {
			return fmt.Errorf("redirect_uri %q: app schemes are disabled on this server", raw)
		}
		if !slices.Contains(s.cfg.RedirectHosts, host) {
			return fmt.Errorf("redirect_uri host %q is not allowed", host)
		}
	}
	return nil
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// describeRedirect is how the consent page names where the user will be sent.
func describeRedirect(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return u.Hostname()
	case "http":
		return "an app on this device (" + u.Host + ")"
	default:
		return "the app that opens " + u.Scheme + ":// links"
	}
}

func (s *server) lookupClient(id string) (*client, error) {
	if !strings.HasPrefix(id, "c.") {
		return nil, errBadToken
	}
	var c client
	if _, err := s.seal.open("client", strings.TrimPrefix(id, "c."), &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// --- authorization endpoint ------------------------------------------------

func (s *server) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	c, err := s.lookupClient(q.Get("client_id"))
	if err != nil {
		errorPage(w, http.StatusBadRequest, "Unknown app registration. Remove this server from your app and add it again.")
		return
	}
	redirect := q.Get("redirect_uri")
	if redirect == "" && len(c.Redirects) == 1 {
		redirect = c.Redirects[0]
	}
	if !slices.Contains(c.Redirects, redirect) {
		errorPage(w, http.StatusBadRequest, "redirect_uri does not match the registered client.")
		return
	}
	// From here on, errors go back to the client.
	fail := func(code, desc string) {
		redirectWith(w, r, redirect, url.Values{"error": {code}, "error_description": {desc}, "state": {q.Get("state")}})
	}
	if q.Get("response_type") != "code" {
		fail("unsupported_response_type", "only response_type=code is supported")
		return
	}
	if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
		fail("invalid_request", "PKCE with code_challenge_method=S256 is required")
		return
	}
	p := pending{
		ClientID:  q.Get("client_id"),
		Redirect:  redirect,
		State:     q.Get("state"),
		Challenge: q.Get("code_challenge"),
		Verifier:  oauth2.GenerateVerifier(),
		Nonce:     oauth2.GenerateVerifier(),
	}
	req, err := s.seal.seal("consent", p, pendingTTL)
	if err != nil {
		serverError(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: flowCookie, Value: p.Nonce, Path: "/oauth", MaxAge: int(pendingTTL.Seconds()),
		HttpOnly: true, Secure: strings.HasPrefix(s.cfg.PublicURL, "https://"), SameSite: http.SameSiteLaxMode,
	})
	w.Header().Set("Cache-Control", "no-store")
	name := c.Name
	if name == "" {
		name = "An app"
	}
	page(w, http.StatusOK, "Connect Google Health", fmt.Sprintf(`
<h1>Connect Google Health</h1>
<p><b>%s</b> wants <b>read-only</b> access to your Google Health data: sleep, activity, heart rate, HRV, SpO2 and body measurements.</p>
<p>After you sign in with Google you'll be sent back to <b>%s</b>.</p>
<p class="muted">The app chose its own name. Only continue if you started connecting it just now.</p>
<form method="post" action="/oauth/authorize">
<input type="hidden" name="request" value="%s">
<button name="decision" value="allow" class="primary">Continue with Google</button>
<button name="decision" value="deny">Cancel</button>
</form>`, html.EscapeString(name), html.EscapeString(describeRedirect(redirect)), req))
}

// consent handles the consent form: on allow it sends the browser to
// Google, on deny it returns access_denied to the client.
func (s *server) consent(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		errorPage(w, http.StatusBadRequest, "Bad request.")
		return
	}
	var p pending
	if _, err := s.seal.open("consent", r.PostForm.Get("request"), &p); err != nil {
		errorPage(w, http.StatusBadRequest, "This sign-in page has expired. Start connecting again from your app.")
		return
	}
	if !s.sameBrowser(r, p.Nonce) {
		errorPage(w, http.StatusForbidden, "This sign-in didn't start in this browser. Start connecting again from your app.")
		return
	}
	if r.PostForm.Get("decision") != "allow" {
		redirectWith(w, r, p.Redirect, url.Values{"error": {"access_denied"}, "error_description": {"the user cancelled"}, "state": {p.State}})
		return
	}
	st, err := s.seal.seal("pending", p, pendingTTL)
	if err != nil {
		serverError(w, err)
		return
	}
	opts := []oauth2.AuthCodeOption{oauth2.AccessTypeOffline, oauth2.ApprovalForce, oauth2.S256ChallengeOption(p.Verifier)}
	if len(s.cfg.AllowedEmails) == 1 {
		opts = append(opts, oauth2.SetAuthURLParam("login_hint", s.cfg.AllowedEmails[0]))
	}
	http.Redirect(w, r, s.google.AuthCodeURL(st, opts...), http.StatusSeeOther)
}

func (s *server) sameBrowser(r *http.Request, nonce string) bool {
	c, err := r.Cookie(flowCookie)
	return err == nil && nonce != "" && subtle.ConstantTimeCompare([]byte(c.Value), []byte(nonce)) == 1
}

// userRef is a stable, non-reversible reference to a user for logs, so
// operators can correlate a user's log lines without logs holding emails.
func (s *server) userRef(email string) string {
	return s.seal.mac("log-user:" + email)[:12]
}

func (s *server) allowed(email string) bool {
	return s.cfg.AllowAnyAccount || slices.Contains(s.cfg.AllowedEmails, email)
}

func (s *server) googleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var p pending
	if _, err := s.seal.open("pending", q.Get("state"), &p); err != nil {
		errorPage(w, http.StatusBadRequest, "This sign-in link has expired. Start connecting again from your app.")
		return
	}
	if !s.sameBrowser(r, p.Nonce) {
		errorPage(w, http.StatusForbidden, "This sign-in didn't start in this browser. Start connecting again from your app.")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: flowCookie, Path: "/oauth", MaxAge: -1})
	fail := func(code, desc string) {
		redirectWith(w, r, p.Redirect, url.Values{"error": {code}, "error_description": {desc}, "state": {p.State}})
	}
	if e := q.Get("error"); e != "" {
		fail("access_denied", "Google sign-in was cancelled: "+e)
		return
	}
	ctx := context.WithValue(r.Context(), oauth2.HTTPClient, s.cfg.HTTPClient)
	tok, err := s.google.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(p.Verifier))
	if err != nil {
		log.Printf("google code exchange failed: %s", googleErrSummary(err))
		fail("server_error", "could not complete Google sign-in")
		return
	}
	email, err := emailFromIDToken(tok)
	if err != nil {
		log.Printf("google id token: %v", err)
		fail("server_error", "Google did not return a verified email")
		return
	}
	if !s.allowed(email) {
		log.Printf("sign-in refused for user %s", s.userRef(email))
		errorPage(w, http.StatusForbidden, "The Google account "+email+" is not allowed to use this server.")
		return
	}
	if tok.RefreshToken == "" {
		fail("server_error", "Google did not return a refresh token; remove the app at myaccount.google.com/permissions and try again")
		return
	}
	s.remember(tok.RefreshToken, tok)
	code, err := s.seal.seal("code", grant{ClientID: p.ClientID, Redirect: p.Redirect, Challenge: p.Challenge, Email: email, GRT: tok.RefreshToken}, codeTTL)
	if err != nil {
		serverError(w, err)
		return
	}
	redirectWith(w, r, p.Redirect, url.Values{"code": {code}, "state": {p.State}})
}

// emailFromIDToken reads the email claim of the id_token returned by
// Google's token endpoint. The token came straight from Google over TLS,
// so (per OIDC Core 3.1.3.7) its signature need not be re-verified.
func emailFromIDToken(tok *oauth2.Token) (string, error) {
	idt, _ := tok.Extra("id_token").(string)
	parts := strings.Split(idt, ".")
	if len(parts) != 3 {
		return "", errors.New("missing id_token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", err
	}
	var claims struct {
		Email         string `json:"email"`
		EmailVerified any    `json:"email_verified"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", err
	}
	if claims.Email == "" || (claims.EmailVerified != true && claims.EmailVerified != "true") {
		return "", errors.New("email missing or not verified")
	}
	return strings.ToLower(claims.Email), nil
}

// --- token endpoint ----------------------------------------------------------

func (s *server) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "bad form body")
		return
	}
	clientID, secret, hasBasic := r.BasicAuth()
	if hasBasic {
		clientID, _ = url.QueryUnescape(clientID)
		secret, _ = url.QueryUnescape(secret)
	} else {
		clientID, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	c, err := s.lookupClient(clientID)
	if err != nil {
		oauthError(w, http.StatusUnauthorized, "invalid_client", "unknown client")
		return
	}
	if (c.Secret || secret != "") && !s.seal.checkMAC("client-secret:"+clientID, secret) {
		oauthError(w, http.StatusUnauthorized, "invalid_client", "bad client secret")
		return
	}

	var g grant
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		if _, err := s.seal.open("code", r.PostForm.Get("code"), &g); err != nil || g.ClientID != clientID {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "authorization code is invalid or expired")
			return
		}
		if ru := r.PostForm.Get("redirect_uri"); ru != "" && ru != g.Redirect {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri mismatch")
			return
		}
		if oauth2.S256ChallengeFromVerifier(r.PostForm.Get("code_verifier")) != g.Challenge {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
			return
		}
	case "refresh_token":
		if _, err := s.seal.open("refresh", r.PostForm.Get("refresh_token"), &g); err != nil || g.ClientID != clientID {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "refresh token is invalid or expired")
			return
		}
		// Make sure Google still honours the login; if not, the client has
		// to run the authorization flow again.
		if _, err := s.googleSource(g.GRT).Token(); err != nil {
			s.forget(g.GRT)
			log.Printf("google refresh failed for user %s: %s", s.userRef(g.Email), googleErrSummary(err))
			oauthError(w, http.StatusBadRequest, "invalid_grant", "the Google login has expired; reconnect")
			return
		}
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "use authorization_code or refresh_token")
		return
	}

	g.Redirect, g.Challenge = "", ""
	at, err1 := s.seal.seal("access", g, accessTokenTTL)
	rt, err2 := s.seal.seal("refresh", g, refreshTokenTTL)
	if err := errors.Join(err1, err2); err != nil {
		serverError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  at,
		"token_type":    "Bearer",
		"expires_in":    int(accessTokenTTL.Seconds()),
		"refresh_token": rt,
		"scope":         scopeName,
	})
}

// --- MCP endpoint ------------------------------------------------------------

func (s *server) verify(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	var g grant
	exp, err := s.seal.open("access", token, &g)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", auth.ErrInvalidToken, err)
	}
	if !s.allowed(g.Email) {
		return nil, fmt.Errorf("%w: account no longer allowed", auth.ErrInvalidToken)
	}
	src := s.googleSource(g.GRT)
	if _, err := src.Token(); err != nil {
		s.forget(g.GRT)
		log.Printf("google token refresh failed for user %s: %s", s.userRef(g.Email), googleErrSummary(err))
		return nil, fmt.Errorf("%w: Google login expired", auth.ErrInvalidToken)
	}
	return &auth.TokenInfo{
		Scopes:     []string{scopeName},
		Expiration: exp,
		UserID:     g.Email,
		Extra:      map[string]any{"google": src},
	}, nil
}

func (s *server) mcpServer(r *http.Request) *mcp.Server {
	ti := auth.TokenInfoFromContext(r.Context())
	src, _ := ti.Extra["google"].(oauth2.TokenSource)
	hc := oauth2.NewClient(context.WithValue(context.Background(), oauth2.HTTPClient, s.cfg.HTTPClient), src)
	return tools.NewServer(tools.Deps{
		Client:   health.NewClient(hc, s.cfg.HealthBaseURL),
		Location: s.cfg.Location,
		Now:      s.cfg.Now,
	}, s.cfg.Version, s.cache)
}

// googleSource returns a cached, auto-refreshing token source for a Google
// refresh token.
func (s *server) googleSource(grt string) oauth2.TokenSource {
	key := hashKey(grt)
	s.mu.Lock()
	defer s.mu.Unlock()
	if src, ok := s.sources[key]; ok {
		return src
	}
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, s.cfg.HTTPClient)
	src := oauth2.ReuseTokenSource(nil, s.google.TokenSource(ctx, &oauth2.Token{RefreshToken: grt}))
	s.sources[key] = src
	return src
}

func (s *server) remember(grt string, tok *oauth2.Token) {
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, s.cfg.HTTPClient)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sources[hashKey(grt)] = oauth2.ReuseTokenSource(tok, s.google.TokenSource(ctx, &oauth2.Token{RefreshToken: grt}))
}

func (s *server) forget(grt string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sources, hashKey(grt))
}

func hashKey(s string) string {
	h := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// --- helpers -------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func oauthError(w http.ResponseWriter, code int, errCode, desc string) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, code, map[string]string{"error": errCode, "error_description": desc})
}

func serverError(w http.ResponseWriter, err error) {
	log.Printf("internal error: %v", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func errorPage(w http.ResponseWriter, code int, msg string) {
	page(w, code, "Can't connect", "<h1>Can't connect</h1><p>"+html.EscapeString(msg)+"</p>")
}

// page writes a small, phone-friendly HTML page. body must already be escaped.
func page(w http.ResponseWriter, code int, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	fmt.Fprintf(w, `<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>%s</title><style>
:root{color-scheme:light dark;--fg:#1f1f1f;--bg:#fff;--muted:#5f6368;--accent:#1a73e8;--line:#dadce0}
@media (prefers-color-scheme:dark){:root{--fg:#e8eaed;--bg:#202124;--muted:#9aa0a6;--accent:#8ab4f8;--line:#5f6368}}
body{font:16px/1.5 system-ui,sans-serif;color:var(--fg);background:var(--bg);max-width:32rem;margin:0 auto;padding:2rem 1rem}
h1{font-size:1.4rem}.muted{color:var(--muted);font-size:.9rem}code{word-break:break-all;background:color-mix(in srgb,var(--line) 40%%,transparent);padding:.1em .3em;border-radius:4px}
form{display:flex;flex-direction:column;gap:.6rem;margin-top:1.5rem}
button{font:inherit;padding:.8rem;border-radius:8px;border:1px solid var(--line);background:transparent;color:var(--fg)}
button.primary{background:var(--accent);border-color:var(--accent);color:var(--bg);font-weight:600}
</style>%s</html>`, html.EscapeString(title), body)
}

func (s *server) landing(w http.ResponseWriter, r *http.Request) {
	page(w, http.StatusOK, "Google Health MCP", fmt.Sprintf(`
<h1>Google Health MCP</h1>
<p>Gives your AI agent read-only access to your Google Health data (Fitbit, Pixel Watch): sleep, activity, heart rate, HRV, SpO2 and weight.</p>
<p><b>To connect:</b> in your agent's settings, add a custom connector (or remote MCP server) with this URL, then sign in with Google when asked:</p>
<p><code>%s/mcp</code></p>
<p class="muted">Version %s. Data is read live from Google each time your agent asks; this server stores nothing.</p>`,
		html.EscapeString(s.cfg.PublicURL), html.EscapeString(s.cfg.Version)))
}

func redirectWith(w http.ResponseWriter, r *http.Request, target string, params url.Values) {
	u, err := url.Parse(target)
	if err != nil {
		errorPage(w, http.StatusBadRequest, "bad redirect_uri")
		return
	}
	q := u.Query()
	for k, v := range params {
		if len(v) > 0 && v[0] != "" {
			q.Set(k, v[0])
		}
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
