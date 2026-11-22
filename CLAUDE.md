# google-health-mcp: notes for coding agents

Go MCP server giving AI agents read-only access to Google Health API data
(Fitbit / Pixel Watch): local stdio mode (`serve`) and a hosted mode for Cloud Run (`serve-http`).
README.md is the user-facing guide; keep it in sync with behaviour changes.

## Checks (same as CI)
`go vet ./... && go test -race -count=1 ./...`, then `bash -n deploy/*.sh && shellcheck deploy/*.sh`

## Design
- Hosted mode (`internal/remote`) stores nothing server-side: client ids, codes and tokens are
  AES-GCM sealed blobs keyed from `MCP_SIGNING_KEY`, with the token kind as associated data.
  Rotating the key revokes every client and token.
- Both modes serve the same tools from `tools.Deps`; only the `*http.Client` behind
  `health.Client` differs (token file locally, the caller's Google token when hosted).
- Tests fake HTTP with `internal/testapi` (Health API) and the `remote.Config` URL overrides
  (Google OAuth). Reuse them instead of adding mocks.

## Things to know
- Never log emails or health data. Use `userRef` (keyed hash) to identify users in logs.
- On Cloud Run use `/health`; the front end swallows `/healthz`.
- `internal/health/catalog.go`: the time field each data type filters on is partly a guess; the
  client falls back to other kinds on INVALID_ARGUMENT. Fix the mapping when a type fails.
- Google OAuth in Testing mode expires refresh tokens after 7 days; users reconnect weekly.
- Deploy scripts must run on macOS bash 3.2 (no associative arrays).
- The CI deploy job only swaps the image; env vars and secrets change only via `deploy/cloudrun.sh`.
