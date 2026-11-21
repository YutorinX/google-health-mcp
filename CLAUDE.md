# google-health-mcp: notes for coding agents

Go MCP server giving AI agents read-only access to Google Health API data
(Fitbit / Pixel Watch). Local stdio mode and a hosted mode for Cloud Run.
README.md is the user-facing guide; keep it in sync with behaviour changes.

## Layout
- `main.go`: commands `login`/`status`/`logout`/`serve` (local stdio) and `serve-http` (hosted)
- `internal/health`: Google Health API v4 client, data-type catalog, date parsing
- `internal/tools`: the 6 MCP tools (official go-sdk)
- `internal/remote`: hosted mode. OAuth 2.1 server for any MCP client (open DCR, PKCE),
  consent page bound to the browser by a SameSite=Lax cookie, Google sign-in with an email
  allowlist or public mode (`ALLOWED_EMAILS=*`), stateless AES-GCM sealed tokens, Streamable HTTP
- `internal/auth`: local-mode OAuth loopback login + token file
- `Dockerfile`, `deploy/cloudrun.sh` (deploy + config), `deploy/setup-github-ci.sh` (WIF bootstrap)
- `.github/workflows/ci.yml`: tests on push/PR; deploys `main` once repo variables are set

## Checks
`go vet ./... && go test -race ./...`; `bash -n deploy/*.sh` and shellcheck for scripts.

## Things to know
- Never log emails or health data. Use `userRef` (keyed hash) to identify users in logs.
- On Cloud Run use `/health`; the front end swallows `/healthz`.
- `internal/health/catalog.go`: the time field each data type filters on is partly a guess; the
  client falls back to other kinds on INVALID_ARGUMENT. Fix the mapping when a type fails.
- Google OAuth in Testing mode expires refresh tokens after 7 days; users reconnect weekly.
- Deploy scripts must run on macOS bash 3.2 (no associative arrays).
