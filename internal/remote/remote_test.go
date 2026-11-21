package remote_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"

	"github.com/shmsi/google-health-mcp/internal/remote"
	"github.com/shmsi/google-health-mcp/internal/testapi"
)

const claudeCallback = "https://claude.ai/api/mcp/auth_callback"

var key = []byte("0123456789abcdef0123456789abcdef")

type env struct {
	t        *testing.T
	srv      *httptest.Server
	google   *httptest.Server
	health   *testapi.Server
	revoked  atomic.Bool
	refreshN atomic.Int32
	cfg      remote.Config
}

func idToken(email string) string {
	enc := base64.RawURLEncoding.EncodeToString
	payload, _ := json.Marshal(map[string]any{"email": email, "email_verified": true})
	return enc([]byte(`{"alg":"RS256"}`)) + "." + enc(payload) + ".sig"
}

func newEnv(t *testing.T) *env {
	e := &env{t: t}
	e.google = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch r.PostForm.Get("grant_type") {
		case "authorization_code":
			if r.PostForm.Get("code_verifier") == "" || r.PostForm.Get("client_secret") != "gsecret" {
				w.WriteHeader(400)
				io.WriteString(w, `{"error":"invalid_request"}`)
				return
			}
			email := map[string]string{"good-code": "Me@Example.com", "other-user": "intruder@example.com"}[r.PostForm.Get("code")]
			json.NewEncoder(w).Encode(map[string]any{
				"access_token": "g-at-1", "token_type": "Bearer", "expires_in": 3600,
				"refresh_token": "g-rt", "id_token": idToken(email),
			})
		case "refresh_token":
			e.refreshN.Add(1)
			if e.revoked.Load() {
				w.WriteHeader(400)
				io.WriteString(w, `{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"access_token": "g-at-2", "token_type": "Bearer", "expires_in": 3600})
		}
	}))
	t.Cleanup(e.google.Close)

	e.health = testapi.New(t, func(r testapi.Request) (int, string) {
		return 200, `{"name":"users/me/identity","healthUserId":"42"}`
	})

	var h http.Handler
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	t.Cleanup(e.srv.Close)
	e.cfg = remote.Config{
		PublicURL:          e.srv.URL,
		GoogleClientID:     "gid",
		GoogleClientSecret: "gsecret",
		AllowedEmails:      []string{"me@example.com"},
		Key:                key,
		HealthScopes:       []string{"https://www.googleapis.com/auth/googlehealth.sleep.readonly"},
		Version:            "test",
		HealthBaseURL:      e.health.URL,
		GoogleAuthURL:      "https://accounts.example/auth",
		GoogleTokenURL:     e.google.URL + "/token",
		HTTPClient:         e.health.Client(),
	}
	var err error
	if h, err = remote.NewHandler(e.cfg); err != nil {
		t.Fatal(err)
	}
	return e
}

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// browser is a cookie-keeping client that doesn't follow redirects, like a
// user's browser stepping through the flow one hop at a time.
func browser() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: noRedirect.CheckRedirect}
}

var requestField = regexp.MustCompile(`name="request" value="([^"]+)"`)

// consentPage opens /oauth/authorize in b and returns the page body and the
// sealed request from its form.
func (e *env) consentPage(b *http.Client, clientID, redirect, challenge string) (string, string) {
	q := url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirect},
		"state": {"claude-state"}, "code_challenge": {challenge}, "code_challenge_method": {"S256"},
		"scope": {"health.read"}, "resource": {e.srv.URL + "/mcp"},
	}
	resp, err := b.Get(e.srv.URL + "/oauth/authorize?" + q.Encode())
	if err != nil {
		e.t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	m := requestField.FindStringSubmatch(string(body))
	if resp.StatusCode != http.StatusOK || m == nil {
		e.t.Fatalf("consent page: %d %s", resp.StatusCode, body)
	}
	return string(body), m[1]
}

func (e *env) decide(b *http.Client, request, decision string) *http.Response {
	resp, err := b.PostForm(e.srv.URL+"/oauth/authorize", url.Values{"request": {request}, "decision": {decision}})
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func (e *env) postJSON(path string, body any) (int, map[string]any) {
	b, _ := json.Marshal(body)
	resp, err := http.Post(e.srv.URL+path, "application/json", strings.NewReader(string(b)))
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (e *env) postForm(path string, form url.Values) (int, map[string]any) {
	resp, err := http.PostForm(e.srv.URL+path, form)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (e *env) register(method string) map[string]any {
	code, out := e.postJSON("/oauth/register", map[string]any{
		"redirect_uris": []string{claudeCallback}, "client_name": "Claude", "token_endpoint_auth_method": method,
	})
	if code != http.StatusCreated {
		e.t.Fatalf("register: %d %v", code, out)
	}
	return out
}

// authorize runs /authorize -> consent (allow) -> fake Google -> callback
// in one browser and returns the final redirect to the client.
func (e *env) authorize(clientID, challenge, googleCode string) *http.Response {
	b := browser()
	_, req := e.consentPage(b, clientID, claudeCallback, challenge)
	resp := e.decide(b, req, "allow")
	if resp.StatusCode != http.StatusSeeOther {
		e.t.Fatalf("consent status %d", resp.StatusCode)
	}
	g, _ := url.Parse(resp.Header.Get("Location"))
	if g.Host != "accounts.example" {
		e.t.Fatalf("consent redirected to %s", g)
	}
	gq := g.Query()
	if gq.Get("access_type") != "offline" || gq.Get("code_challenge") == "" ||
		!strings.Contains(gq.Get("scope"), "openid email https://www.googleapis.com/auth/googlehealth.sleep.readonly") ||
		gq.Get("redirect_uri") != e.srv.URL+"/oauth/google/callback" {
		e.t.Errorf("google auth params: %v", gq)
	}
	if len(e.cfg.AllowedEmails) == 1 && gq.Get("login_hint") != "me@example.com" {
		e.t.Errorf("login_hint = %q", gq.Get("login_hint"))
	}
	cb := url.Values{"code": {googleCode}, "state": {gq.Get("state")}}
	resp, err := b.Get(e.srv.URL + "/oauth/google/callback?" + cb.Encode())
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

// login performs the whole flow and returns the token response.
func (e *env) login() (clientID string, tokens map[string]any) {
	clientID = e.register("none")["client_id"].(string)
	verifier := oauth2.GenerateVerifier()
	resp := e.authorize(clientID, oauth2.S256ChallengeFromVerifier(verifier), "good-code")
	back, _ := url.Parse(resp.Header.Get("Location"))
	if !strings.HasPrefix(back.String(), claudeCallback) || back.Query().Get("state") != "claude-state" || back.Query().Get("code") == "" {
		e.t.Fatalf("callback redirect = %s", back)
	}
	code, out := e.postForm("/oauth/token", url.Values{
		"grant_type": {"authorization_code"}, "code": {back.Query().Get("code")}, "client_id": {clientID},
		"redirect_uri": {claudeCallback}, "code_verifier": {verifier},
	})
	if code != 200 {
		e.t.Fatalf("token: %d %v", code, out)
	}
	return clientID, out
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func (e *env) mcpSession(token string) (*mcp.ClientSession, error) {
	c := mcp.NewClient(&mcp.Implementation{Name: "claude-test", Version: "1"}, nil)
	return c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:   e.srv.URL + "/mcp",
		HTTPClient: &http.Client{Transport: bearer{token}},
	}, nil)
}

func TestDiscovery(t *testing.T) {
	e := newEnv(t)
	resp, err := http.Post(e.srv.URL+"/mcp", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 || !strings.Contains(resp.Header.Get("WWW-Authenticate"), `resource_metadata="`+e.srv.URL+"/.well-known/oauth-protected-resource/mcp") {
		t.Errorf("unauthenticated: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	var prm map[string]any
	r2, _ := http.Get(e.srv.URL + "/.well-known/oauth-protected-resource/mcp")
	json.NewDecoder(r2.Body).Decode(&prm)
	r2.Body.Close()
	if prm["resource"] != e.srv.URL+"/mcp" || prm["authorization_servers"].([]any)[0] != e.srv.URL {
		t.Errorf("prm = %v", prm)
	}
	var as map[string]any
	r3, _ := http.Get(e.srv.URL + "/.well-known/oauth-authorization-server")
	json.NewDecoder(r3.Body).Decode(&as)
	r3.Body.Close()
	if as["registration_endpoint"] != e.srv.URL+"/oauth/register" || as["issuer"] != e.srv.URL {
		t.Errorf("as = %v", as)
	}
}

func TestFullFlowAndToolCall(t *testing.T) {
	e := newEnv(t)
	clientID, tok := e.login()
	if tok["token_type"] != "Bearer" || tok["refresh_token"] == nil || tok["expires_in"].(float64) != 3600 {
		t.Fatalf("tokens = %v", tok)
	}

	cs, err := e.mcpSession(tok["access_token"].(string))
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil || len(tools.Tools) != 6 {
		t.Fatalf("tools: %v %v", tools, err)
	}
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_account_info"})
	if err != nil || res.IsError {
		t.Fatalf("call: %v %+v", err, res)
	}
	if text := res.Content[0].(*mcp.TextContent).Text; !strings.Contains(text, `"healthUserId":"42"`) {
		t.Errorf("result = %s", text)
	}
	if e.refreshN.Load() != 0 {
		t.Errorf("unexpected Google refreshes: %d", e.refreshN.Load())
	}

	// Refresh works and rotates the refresh token.
	code, out := e.postForm("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok["refresh_token"].(string)}, "client_id": {clientID}})
	if code != 200 || out["access_token"] == tok["access_token"] {
		t.Fatalf("refresh: %d %v", code, out)
	}
}

func TestHealthAPIGetsGoogleToken(t *testing.T) {
	e := newEnv(t)
	var gotAuth atomic.Value
	e.health.Handler = func(r testapi.Request) (int, string) { return 200, `{}` }
	wrapped := e.cfg.HTTPClient
	e.cfg.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasPrefix(r.URL.String(), e.health.URL) {
			gotAuth.Store(r.Header.Get("Authorization"))
		}
		return wrapped.Transport.RoundTrip(r)
	})}
	h, err := remote.NewHandler(e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.srv.Config.Handler = h
	_, tok := e.login()
	cs, err := e.mcpSession(tok["access_token"].(string))
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_account_info"})
	if gotAuth.Load() != "Bearer g-at-1" {
		t.Errorf("health API auth = %v", gotAuth.Load())
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestWrongGoogleAccountIsRefused(t *testing.T) {
	e := newEnv(t)
	clientID := e.register("none")["client_id"].(string)
	resp := e.authorize(clientID, oauth2.S256ChallengeFromVerifier(oauth2.GenerateVerifier()), "other-user")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, location = %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestPKCEAndClientChecks(t *testing.T) {
	e := newEnv(t)
	clientID := e.register("none")["client_id"].(string)
	verifier := oauth2.GenerateVerifier()
	back, _ := url.Parse(e.authorize(clientID, oauth2.S256ChallengeFromVerifier(verifier), "good-code").Header.Get("Location"))
	authCode := back.Query().Get("code")

	other := e.register("none")["client_id"].(string)
	for name, form := range map[string]url.Values{
		"wrong verifier":  {"grant_type": {"authorization_code"}, "code": {authCode}, "client_id": {clientID}, "code_verifier": {oauth2.GenerateVerifier()}},
		"other client":    {"grant_type": {"authorization_code"}, "code": {authCode}, "client_id": {other}, "code_verifier": {verifier}},
		"code as refresh": {"grant_type": {"refresh_token"}, "refresh_token": {authCode}, "client_id": {clientID}},
	} {
		if code, out := e.postForm("/oauth/token", form); code != 400 || out["error"] != "invalid_grant" {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	if code, out := e.postForm("/oauth/token", url.Values{"grant_type": {"authorization_code"}, "client_id": {"c.forged"}}); code != 401 || out["error"] != "invalid_client" {
		t.Errorf("forged client: %d %v", code, out)
	}
}

func TestConfidentialClientNeedsSecret(t *testing.T) {
	e := newEnv(t)
	reg := e.register("client_secret_post")
	id, secret := reg["client_id"].(string), reg["client_secret"].(string)
	verifier := oauth2.GenerateVerifier()
	back, _ := url.Parse(e.authorize(id, oauth2.S256ChallengeFromVerifier(verifier), "good-code").Header.Get("Location"))
	form := url.Values{"grant_type": {"authorization_code"}, "code": {back.Query().Get("code")}, "client_id": {id}, "code_verifier": {verifier}}
	if code, _ := e.postForm("/oauth/token", form); code != 401 {
		t.Errorf("without secret: %d", code)
	}
	form.Set("client_secret", secret)
	if code, out := e.postForm("/oauth/token", form); code != 200 {
		t.Errorf("with secret: %d %v", code, out)
	}
}

func TestRegisterRedirects(t *testing.T) {
	e := newEnv(t)
	reg := func(u string) int {
		code, _ := e.postJSON("/oauth/register", map[string]any{"redirect_uris": []string{u}, "token_endpoint_auth_method": "none"})
		return code
	}
	for _, u := range []string{
		"https://claude.ai/api/mcp/auth_callback",
		"https://chatgpt.com/connector_platform_oauth_redirect",
		"https://vscode.dev/redirect",
		"cursor://anysphere.cursor-retrieval/oauth/user-health/callback",
		"com.example.app:/oauth2redirect",
		"http://localhost:33418/callback",
		"http://127.0.0.1:6274/oauth/callback",
	} {
		if code := reg(u); code != 201 {
			t.Errorf("%s rejected (%d)", u, code)
		}
	}
	for _, u := range []string{
		"http://evil.example/cb", "http://claude.ai/api/mcp/auth_callback", "javascript:alert(1)",
		"JavaScript:alert(1)", "data:text/html,hi", "file:///etc/passwd", "https://ok.example/cb#frag", "https:///nohost", "relative/path",
	} {
		if code := reg(u); code != 400 {
			t.Errorf("%s accepted (%d)", u, code)
		}
	}

	// Strict mode: only listed hosts, no app schemes.
	e.cfg.RedirectHosts = []string{"claude.ai", "localhost"}
	h, err := remote.NewHandler(e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.srv.Config.Handler = h
	for u, want := range map[string]int{
		"https://claude.ai/api/mcp/auth_callback":               201,
		"http://localhost:1234/cb":                              201,
		"https://chatgpt.com/connector_platform_oauth_redirect": 400,
		"cursor://anysphere.cursor-retrieval/oauth/cb":          400,
	} {
		if code := reg(u); code != want {
			t.Errorf("strict %s: %d, want %d", u, code, want)
		}
	}
}

func TestConsentPage(t *testing.T) {
	e := newEnv(t)
	_, out := e.postJSON("/oauth/register", map[string]any{
		"redirect_uris": []string{"cursor://anysphere.cursor-retrieval/cb"}, "client_name": `<script>x</script>Cursor`, "token_endpoint_auth_method": "none",
	})
	id := out["client_id"].(string)
	b := browser()
	body, req := e.consentPage(b, id, "cursor://anysphere.cursor-retrieval/cb", oauth2.S256ChallengeFromVerifier(oauth2.GenerateVerifier()))
	if strings.Contains(body, "<script>") || !strings.Contains(body, "&lt;script&gt;x&lt;/script&gt;Cursor") {
		t.Errorf("client name not escaped: %s", body)
	}
	if !strings.Contains(body, "cursor://") {
		t.Errorf("consent page doesn't name the redirect: %s", body)
	}

	// Deny goes back to the client with access_denied.
	resp := e.decide(b, req, "deny")
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "cursor://anysphere.cursor-retrieval/cb?") ||
		!strings.Contains(loc, "error=access_denied") || !strings.Contains(loc, "state=claude-state") {
		t.Errorf("deny location %q", loc)
	}
}

func TestConsentIsBoundToBrowser(t *testing.T) {
	e := newEnv(t)
	id := e.register("none")["client_id"].(string)
	attacker := browser()
	_, req := e.consentPage(attacker, id, claudeCallback, oauth2.S256ChallengeFromVerifier(oauth2.GenerateVerifier()))

	// Another site auto-submitting the consent form in the victim's browser.
	victim := browser()
	if resp := e.decide(victim, req, "allow"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-browser consent: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}

	// The attacker approves in their own browser and sends the victim the
	// raw Google link, skipping the consent page.
	resp := e.decide(attacker, req, "allow")
	g, _ := url.Parse(resp.Header.Get("Location"))
	cb := url.Values{"code": {"good-code"}, "state": {g.Query().Get("state")}}
	resp, err := victim.Get(e.srv.URL + "/oauth/google/callback?" + cb.Encode())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || resp.Header.Get("Location") != "" {
		t.Errorf("callback in other browser: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	// A consent request can't be used as the Google state directly.
	cb.Set("state", req)
	resp, _ = attacker.Get(e.srv.URL + "/oauth/google/callback?" + cb.Encode())
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("consent request as state: %d", resp.StatusCode)
	}
}

func TestPublicModeAcceptsAnyAccount(t *testing.T) {
	e := newEnv(t)
	e.cfg.AllowAnyAccount = true
	h, err := remote.NewHandler(e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.srv.Config.Handler = h
	e.cfg.AllowedEmails = nil // as NewHandler sees it; no login_hint expected
	clientID := e.register("none")["client_id"].(string)
	verifier := oauth2.GenerateVerifier()
	resp := e.authorize(clientID, oauth2.S256ChallengeFromVerifier(verifier), "other-user")
	back, _ := url.Parse(resp.Header.Get("Location"))
	code, out := e.postForm("/oauth/token", url.Values{
		"grant_type": {"authorization_code"}, "code": {back.Query().Get("code")}, "client_id": {clientID}, "code_verifier": {verifier},
	})
	if code != 200 {
		t.Fatalf("token: %d %v", code, out)
	}
	cs, err := e.mcpSession(out["access_token"].(string))
	if err != nil {
		t.Fatalf("public-mode MCP session: %v", err)
	}
	cs.Close()
}

func TestHealthEndpoints(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{"/health", "/healthz"} {
		resp, err := http.Get(e.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("%s: %d", path, resp.StatusCode)
		}
	}
}

func TestLandingPage(t *testing.T) {
	e := newEnv(t)
	resp, err := http.Get(e.srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), e.srv.URL+"/mcp") || resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Errorf("landing: %s %v", body, resp.Header)
	}
}

func TestBadTokensAreRejected(t *testing.T) {
	e := newEnv(t)
	_, tok := e.login()
	for name, token := range map[string]string{
		"garbage":           "nope",
		"refresh as access": tok["refresh_token"].(string),
		"tampered":          tok["access_token"].(string)[:20] + "AAAA" + tok["access_token"].(string)[24:],
	} {
		req, _ := http.NewRequest("POST", e.srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Errorf("%s: status %d", name, resp.StatusCode)
		}
	}
}

func TestTokensSurviveRestartAndExpiredGoogleLoginForcesReconnect(t *testing.T) {
	e := newEnv(t)
	clientID, tok := e.login()

	// "Restart": a fresh handler with the same key has no cache.
	h, err := remote.NewHandler(e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.srv.Config.Handler = h
	cs, err := e.mcpSession(tok["access_token"].(string))
	if err != nil {
		t.Fatalf("after restart: %v", err)
	}
	cs.Close()
	if e.refreshN.Load() != 1 {
		t.Errorf("Google refreshes = %d, want 1", e.refreshN.Load())
	}

	// Google revokes (e.g. the 7-day Testing-mode expiry).
	e.revoked.Store(true)
	h, _ = remote.NewHandler(e.cfg)
	e.srv.Config.Handler = h
	if code, out := e.postForm("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok["refresh_token"].(string)}, "client_id": {clientID}}); code != 400 || out["error"] != "invalid_grant" {
		t.Errorf("refresh after revoke: %d %v", code, out)
	}
	if _, err := e.mcpSession(tok["access_token"].(string)); err == nil {
		t.Error("expected MCP connect to fail after revoke")
	}
}

func TestExpiredAccessToken(t *testing.T) {
	e := newEnv(t)
	now := time.Now()
	e.cfg.Now = func() time.Time { return now }
	h, _ := remote.NewHandler(e.cfg)
	e.srv.Config.Handler = h
	_, tok := e.login()
	now = now.Add(2 * time.Hour)
	if _, err := e.mcpSession(tok["access_token"].(string)); err == nil {
		t.Error("expected expired token to be rejected")
	}
}

func TestConfigValidation(t *testing.T) {
	base := remote.Config{PublicURL: "https://x.run.app", GoogleClientID: "a", GoogleClientSecret: "b", AllowedEmails: []string{"me@example.com"}, Key: key}
	for name, mut := range map[string]func(*remote.Config){
		"http url":  func(c *remote.Config) { c.PublicURL = "http://x.run.app" },
		"no emails": func(c *remote.Config) { c.AllowedEmails = nil },
		"short key": func(c *remote.Config) { c.Key = []byte("short") },
		"no client": func(c *remote.Config) { c.GoogleClientSecret = "" },
	} {
		c := base
		c.AllowedEmails = append([]string{}, base.AllowedEmails...)
		mut(&c)
		if _, err := remote.NewHandler(c); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if _, err := remote.NewHandler(base); err != nil {
		t.Errorf("valid config: %v", err)
	}
}

func TestAuthorizeRejectsUnregisteredRedirect(t *testing.T) {
	e := newEnv(t)
	clientID := e.register("none")["client_id"].(string)
	q := url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {"https://claude.com/api/mcp/auth_callback"},
		"code_challenge": {oauth2.S256ChallengeFromVerifier(oauth2.GenerateVerifier())}, "code_challenge_method": {"S256"},
	}
	resp, err := noRedirect.Get(e.srv.URL + "/oauth/authorize?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Location") != "" {
		t.Errorf("status %d location %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	// Missing PKCE is refused back to the client.
	q.Set("redirect_uri", claudeCallback)
	q.Del("code_challenge")
	resp, _ = noRedirect.Get(e.srv.URL + "/oauth/authorize?" + q.Encode())
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, claudeCallback) || !strings.Contains(loc, "error=invalid_request") {
		t.Errorf("no-PKCE location %q", loc)
	}
}
