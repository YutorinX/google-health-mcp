# google-health-mcp

Ask your AI agent about your Fitbit or Pixel Watch data:

> *"How did I sleep this week compared with last week?"*
> *"Plot my resting heart rate and HRV over the last 60 days."*
> *"What was my heart rate like during yesterday's run?"*

`google-health-mcp` is a small, self-hosted [MCP](https://modelcontextprotocol.io)
server that gives AI agents **read-only** access to your
[Google Health API](https://developers.google.com/health) data: sleep, steps,
activity, heart rate, HRV, SpO2, breathing rate, weight and workouts. It works
with Claude, ChatGPT, Cursor, VS Code, Claude Code and any other MCP client that
supports remote servers with OAuth, including on your phone.

You run it yourself, in your own Google Cloud project. Your data goes from
Google to your server to your agent, and nowhere else.

> **Unofficial.** Not affiliated with Google or Fitbit. Not medical advice.

---

## Contents

- [Which setup should I pick?](#which-setup-should-i-pick)
- [Hosted setup (phone, web and desktop)](#hosted-setup-phone-web-and-desktop)
- [Local setup (desktop only)](#local-setup-desktop-only)
- [Connect your agent](#connect-your-agent)
- [What you can ask: the tools](#what-you-can-ask-the-tools)
- [Troubleshooting](#troubleshooting)
- [How it works, and what's stored](#how-it-works-and-whats-stored)
- [Sharing your server with others](#sharing-your-server-with-others)
- [Automatic deploys from GitHub (optional)](#automatic-deploys-from-github-optional)
- [Configuration reference](#configuration-reference)
- [Development](#development)

---

## Which setup should I pick?

| | **Hosted** (Google Cloud Run) | **Local** (on your computer) |
|---|---|---|
| Works on your phone | ✅ | ❌ |
| Works on the web (claude.ai, chatgpt.com) | ✅ | ❌ |
| Works in desktop apps and coding tools | ✅ | ✅ |
| Needs | A Google Cloud project **with billing**, `gcloud` | Go 1.26+ |
| Cost | Usually free for one person (scales to zero) | Free |
| Time | ~20 minutes | ~15 minutes |

Most people want **hosted**: set it up once and it works in every app,
including the phone.

Either way, you need a Google account whose Fitbit or Pixel Watch data lives in
Google Health. If you still sign in to the Fitbit app with an old Fitbit
account, finish moving it to Google first.

---

## Hosted setup (phone, web and desktop)

You'll create a Google Cloud project, set up a Google sign-in screen for it,
and run one deploy script. Then you add the server's URL to your agent.

### What you need

- A Google account (the one with your Fitbit or Pixel Watch data).
- A **Google Cloud project with billing enabled**. Create one at
  [console.cloud.google.com/projectcreate](https://console.cloud.google.com/projectcreate),
  then link a billing account under **Billing**. Cloud Run's free tier normally
  covers one person's use, but billing must be on.
  Project IDs can't contain the word "google". Something like `health-mcp-yourname` works.
- The [`gcloud` CLI](https://cloud.google.com/sdk/docs/install), signed in with
  `gcloud auth login`.
- `git`, `bash`, `curl` and `openssl`. macOS and Linux already have them.
  On Windows, use [Cloud Shell](https://shell.cloud.google.com), which has everything.

### Step 1: Get the code and find your server's URL

```sh
git clone https://github.com/shmsi/google-health-mcp.git
cd google-health-mcp
PROJECT_ID=your-project-id ./deploy/cloudrun.sh url
```

This doesn't change anything yet. It prints three lines. Keep them handy:

```
Service URL:            https://google-health-mcp-123456789.europe-north1.run.app
MCP URL (for agents):   https://google-health-mcp-123456789.europe-north1.run.app/mcp
OAuth redirect URI:     https://google-health-mcp-123456789.europe-north1.run.app/oauth/google/callback
```

The default region is `europe-north1` (Finland). To use another, put
`REGION=us-central1` (or similar) in front of **every** command in this guide.

### Step 2: Turn on the Google Health API

Open [APIs & Services → Library](https://console.cloud.google.com/apis/library/health.googleapis.com),
check the project picker at the top shows your project, and click **Enable**.
(The deploy script also tries this, but doing it now makes the scopes show up
in step 3.)

### Step 3: Create the Google sign-in screen

This is the screen you see when you connect your agent. All of it is in
**Google Auth Platform** in the Cloud Console.

1. **[Branding](https://console.cloud.google.com/auth/branding)**. If you see
   **Get started**, click it. Enter an app name (for example *Health MCP*) and
   your email, choose **External** as the audience, and finish the wizard.
2. **[Audience](https://console.cloud.google.com/auth/audience)**.
   - Leave **Publishing status** on **Testing**. Don't publish the app.
   - Under **Test users**, click **Add users** and add your own Google account.
     Only test users can sign in.
3. **[Data Access](https://console.cloud.google.com/auth/scopes)** → **Add or remove scopes**.
   - Tick `openid` and `.../auth/userinfo.email`.
   - Under **Manually add scopes**, paste these five lines and click **Add to table**:
     ```
     https://www.googleapis.com/auth/googlehealth.activity_and_fitness.readonly
     https://www.googleapis.com/auth/googlehealth.health_metrics_and_measurements.readonly
     https://www.googleapis.com/auth/googlehealth.sleep.readonly
     https://www.googleapis.com/auth/googlehealth.profile.readonly
     https://www.googleapis.com/auth/googlehealth.settings.readonly
     ```
   - Click **Update**, then **Save**.
4. **[Clients](https://console.cloud.google.com/auth/clients)** → **Create client**.
   - Application type: **Web application**.
   - Under **Authorized redirect URIs**, add the **OAuth redirect URI** from step 1.
     It must match exactly.
   - Click **Create**. Copy the **Client ID** and the **Client secret**.

### Step 4: Deploy

Replace the values and run:

```sh
PROJECT_ID=your-project-id \
ALLOWED_EMAILS=you@gmail.com \
GOOGLE_OAUTH_CLIENT_ID=1234-abc.apps.googleusercontent.com \
GOOGLE_OAUTH_CLIENT_SECRET=GOCSPX-your-secret \
GOOGLE_HEALTH_TZ=Europe/Stockholm \
./deploy/cloudrun.sh deploy
```

- `ALLOWED_EMAILS`: the Google account(s) allowed to connect, comma-separated.
- `GOOGLE_HEALTH_TZ`: your [time zone](https://en.wikipedia.org/wiki/List_of_tz_database_time_zones),
  so "today" and "last night" mean what you expect. The default is UTC.

The first run takes about 5 minutes. The script enables the APIs it needs,
creates an encryption key, stores the client secret in Secret Manager, builds
the server with Cloud Build and deploys it. It ends with:

```
==> Health check OK

Done. Add this as a custom connector / remote MCP server in your agent:
  https://google-health-mcp-123456789.europe-north1.run.app/mcp
```

**Check:** open the **Service URL** in a browser. You should see a short page
titled *Google Health MCP* with your MCP URL on it.

### Step 5: Connect your agent

Add the **MCP URL** (ending in `/mcp`) to your agent. See
[Connect your agent](#connect-your-agent) for where that setting is in each app.

When you click **Connect**:
1. This server's own page asks you to confirm the app that's connecting. Click **Continue with Google**.
2. Google warns that it **hasn't verified this app**. That's expected for a
   personal app in Testing mode, and the app is yours. Click **Continue**.
3. Choose your account, allow the permissions, and you're sent back to your agent.

Then ask: *"How did I sleep last night?"*

### Keeping it running

- **Reconnect weekly.** While the sign-in screen is in Testing mode, Google
  ends the sign-in after **7 days**. Your agent then asks you to connect again,
  which takes a few seconds. The only way around this is
  [publishing the app](#sharing-your-server-with-others), which needs Google's verification.
- **Updating.** `git pull`, then run the deploy command from step 4 again. You can
  leave out `GOOGLE_OAUTH_CLIENT_SECRET`, because it's already stored. Keep
  `GOOGLE_HEALTH_TZ`, or it goes back to UTC. To deploy on every push instead, see
  [Automatic deploys](#automatic-deploys-from-github-optional).
- **Stopping it.** Delete the Cloud Run service, or the whole project, in the
  Cloud Console. Revoke the app's access at
  [myaccount.google.com/permissions](https://myaccount.google.com/permissions).

---

## Local setup (desktop only)

The server runs on your computer, and your desktop app starts it when needed.
No Cloud Run, no billing.

### Step 1: Google Cloud project and sign-in screen

1. Create a project at [console.cloud.google.com/projectcreate](https://console.cloud.google.com/projectcreate). Billing isn't needed.
2. [Enable the Google Health API](https://console.cloud.google.com/apis/library/health.googleapis.com).
3. Set up **Branding**, **Audience** (Testing, add yourself as a test user) and
   **Data Access** (the five `googlehealth` scopes) exactly as in
   [hosted step 3](#step-3-create-the-google-sign-in-screen). You don't need
   `openid` or `email` locally.
4. **[Clients](https://console.cloud.google.com/auth/clients)** → **Create client** →
   Application type **Desktop app** → **Create**, then **Download JSON**.

### Step 2: Save the client file

Save the downloaded file as `credentials.json` in this folder (create it if needed):

| OS | Folder |
|---|---|
| macOS | `~/Library/Application Support/google-health-mcp/` |
| Linux | `~/.config/google-health-mcp/` |
| Windows | `%AppData%\google-health-mcp\` |

Alternatively, set `GOOGLE_HEALTH_CLIENT_ID` and `GOOGLE_HEALTH_CLIENT_SECRET`
instead of using the file. Set `GOOGLE_HEALTH_MCP_DIR` to use another folder.

### Step 3: Install and sign in

```sh
go install github.com/shmsi/google-health-mcp@latest
google-health-mcp login     # opens your browser; tick every permission
google-health-mcp status    # checks the connection
```

`go install` puts the binary in `$(go env GOPATH)/bin`. Make sure that's on
your `PATH`, or use the full path below.

### Step 4: Add it to your app

Most desktop apps (Claude Desktop, Cursor and others) use this format. In
Claude Desktop it goes in `claude_desktop_config.json`, and in Cursor in
`~/.cursor/mcp.json`:

```json
{
  "mcpServers": {
    "google-health": {
      "command": "/full/path/to/google-health-mcp",
      "env": { "GOOGLE_HEALTH_TZ": "Europe/Stockholm" }
    }
  }
}
```

VS Code uses `"servers"` instead of `"mcpServers"` and adds `"type": "stdio"`.
For Claude Code:

```sh
claude mcp add google-health -e GOOGLE_HEALTH_TZ=Europe/Stockholm -- /full/path/to/google-health-mcp
```

Restart the app and ask: *"How did I sleep last night?"*

In Testing mode the sign-in lasts 7 days. When it expires, run
`google-health-mcp login` again.

---

## Connect your agent

Your agent needs the **MCP URL**: your service URL followed by `/mcp`.

| Client | Where |
|---|---|
| **Claude** (web, phone, desktop) | **Settings → Connectors → Add custom connector**. Add it on claude.ai and it appears in the phone and desktop apps too. |
| **ChatGPT** | Custom connectors / apps in **Settings**. Depending on your plan, you may need to turn on developer mode. |
| **Claude Code** | `claude mcp add --transport http google-health https://…/mcp` |
| **VS Code** | `.vscode/mcp.json`: `{"servers": {"google-health": {"type": "http", "url": "https://…/mcp"}}}` |
| **Cursor** | `~/.cursor/mcp.json`: `{"mcpServers": {"google-health": {"url": "https://…/mcp"}}}` |
| **Anything else** | Any client with remote MCP (Streamable HTTP) and OAuth support. Leave client ID and secret empty: the server registers clients automatically. |

`deploy/cloudrun.sh` lets only Claude on the web (`claude.ai`, `claude.com`)
register by default. For other clients, redeploy with their redirect hosts added
to `ALLOWED_REDIRECT_HOSTS`, such as `localhost,127.0.0.1` for Claude Code
(or use Claude Code with the local server instead).
Clients that redirect to an app scheme, such as Cursor, are refused while the
list is set.

Most phone apps can't add connectors from the phone. Add it on the web version
once, and it shows up on your phone. Menu names change often, so check your
app's docs if these don't match.

---

## What you can ask: the tools

| Tool | What it's for |
|---|---|
| `get_daily_overview` | One call for 1–31 days: steps, distance, floors, Active Zone Minutes and calories, plus resting HR, HRV, SpO2, breathing rate, sleep sessions and workouts |
| `get_daily_totals` | Per-day (or per-N-day) totals for trends, such as steps over the last 90 days or weekly Active Zone Minutes |
| `get_data_points` | Raw records: sleep sessions with stages, workouts, daily summaries, weight. Uses the *reconciled* stream, which merges overlapping devices the way the Fitbit app does. |
| `get_rollup` | Time buckets within a day, such as heart rate every 5 minutes during a run, or steps per hour |
| `get_account_info` | Connection check, profile, settings, paired devices, and the server's idea of "today" |
| `list_data_types` | The supported data types and which tools work with each |

Dates can be `YYYY-MM-DD`, `today`, `yesterday` or relative like `-7d`. Sleep
counts for the day it **ended**, so "last night" is today's date.

More prompts to try:
- *"Compare my average steps this month with last month."*
- *"On which days last week did I get less than 7 hours of sleep?"*
- *"Show my weight trend since January."*
- *"How much time did I spend in deep sleep on average this week?"*

---

## Troubleshooting

**"Access blocked: … has not completed the Google verification process" / `access_denied`.**
The Google account isn't a test user. Add it under
[Audience → Test users](https://console.cloud.google.com/auth/audience).

**`Error 400: redirect_uri_mismatch`.**
The redirect URI on your OAuth client doesn't exactly match the server's. Run
`./deploy/cloudrun.sh url` again and copy the **OAuth redirect URI** into the
client under [Clients](https://console.cloud.google.com/auth/clients).

**"The Google account … is not allowed to use this server."**
That account isn't in `ALLOWED_EMAILS`. Re-run the deploy with it added.

**It worked for a week, then stopped.**
The 7-day sign-in limit of Testing mode. Reconnect in your agent.

**`ACCOUNT_NOT_LINKED` from `get_account_info`.**
Your Fitbit or Pixel Watch data isn't attached to that Google account yet. Finish
moving your Fitbit account to Google in the Fitbit app.

**The deploy fails with a billing or permission error.**
Make sure billing is enabled on the project, and that your `gcloud` account is
an owner or editor of it. If Cloud Build reports a permission error, grant
`roles/run.builder` to the `<project-number>-compute@developer.gserviceaccount.com`
service account under **IAM**.

**A question fails for one kind of data, while others work.**
The API filters each data type on a different time field, and the mapping in
`internal/health/catalog.go` is partly a best guess. Please
[open an issue](https://github.com/shmsi/google-health-mcp/issues) with the
data type and the error message.

**Checking the server's logs.**
```sh
gcloud run services logs read google-health-mcp --region europe-north1 --project your-project-id
```
The logs never contain email addresses. Users show up as short anonymous IDs.

---

## How it works, and what's stored

```
Agent ──OAuth (DCR + PKCE)──▶ google-health-mcp ──Google sign-in──▶ Google
  │                         (consent page)   │
  └──── MCP over HTTPS ─────────────────────▶└──── Google Health API
```

- **Nothing about your health is stored.** Each time your agent asks, the server
  reads from Google, formats the answer and passes it straight through.
- **Your Google sign-in is held by your agent, encrypted.** After you sign in,
  Google gives the server a refresh token. The server encrypts it (AES-256-GCM)
  inside the tokens it hands your agent, and keeps no copy. If the server
  restarts, your connection keeps working.
- **The encryption key** is generated at deploy time and kept in Secret Manager,
  readable only by the server's own service account. Rotating it signs every
  agent out.
- **Logs** contain no emails or health data. Users appear as short anonymous IDs.
- **Read-only.** The server never asks Google for write access.
- **Once an answer reaches your agent,** it's part of your chat history with that
  provider (Anthropic, OpenAI…) under their terms.

**Why the consent page?** Any MCP client can register with the server: that's
how "just paste the URL" works. To stop someone tricking you into connecting
*their* app, every connection first shows which app is asking and where you'll
be sent back. The page is tied to your browser, so it can't be skipped by
sending you a Google sign-in link directly.

---

## Sharing your server with others

To let friends or family use your server:

1. Add each person's Google account as a **test user** under
   [Audience](https://console.cloud.google.com/auth/audience). Testing mode allows up to 100.
2. Re-deploy with their emails in `ALLOWED_EMAILS`, or use `ALLOWED_EMAILS='*'`
   to accept every account Google lets sign in (in Testing mode, that's only
   your test users).

Each person signs in with their own Google account and sees only their own
data. Their sign-in lives in their own agent, not on your server.

Opening it to **anyone**, without adding them by hand and without the weekly
reconnect, means publishing the app. The Google Health scopes are *restricted*,
so publishing requires Google's verification: a privacy policy, a verified
domain and usually a yearly third-party security assessment. That's meant for
businesses, not personal servers.

---

## Automatic deploys from GitHub (optional)

If you fork this repo, it can deploy itself on every push to `main`:

```sh
PROJECT_ID=your-project-id GITHUB_REPO=you/google-health-mcp ./deploy/setup-github-ci.sh
```

Run it after the first deploy. It sets up keyless authentication
(Workload Identity Federation, trusted only for your repo's `main` branch) and a
deployer service account with minimal permissions, and it sets the repository
variables the workflow needs (using `gh` if installed). From then on, every push
to `main` runs the tests, builds the image, deploys it and checks it's healthy.
Until those variables are set, the deploy job is skipped.

CI only replaces the server's code. Settings such as allowed emails and the time
zone stay as the deploy script set them.

---

## Configuration reference

**Hosted server** (`google-health-mcp serve-http`), set by `deploy/cloudrun.sh`:

| Variable | Meaning |
|---|---|
| `PUBLIC_URL` | The server's public https URL |
| `ALLOWED_EMAILS` | Google accounts allowed to connect, comma-separated, or `*` for any account Google allows |
| `GOOGLE_OAUTH_CLIENT_ID`, `GOOGLE_OAUTH_CLIENT_SECRET` | The Web application OAuth client |
| `MCP_SIGNING_KEY` | Base64, at least 32 random bytes. Encrypts all issued tokens. |
| `GOOGLE_HEALTH_TZ` | Time zone for "today" (default UTC) |
| `GOOGLE_HEALTH_EXTRA_SCOPES` | Extra read scopes, such as `nutrition.readonly,ecg.readonly`. Add them to the sign-in screen too. |
| `ALLOWED_REDIRECT_HOSTS` | Strict mode: only these hosts may register as clients, such as `claude.ai,claude.com,localhost,127.0.0.1`. App schemes like `cursor://` are refused. Unset on the server, any https, loopback or app-scheme redirect is allowed; `deploy/cloudrun.sh` always sets it (default `claude.ai,claude.com`). |
| `PORT` | Listen port (default 8080) |

`deploy/cloudrun.sh` also accepts `REGION` (default `europe-north1`),
`SERVICE` (default `google-health-mcp`) and `MAX_INSTANCES` (default 1).

**Local server** (`google-health-mcp`):

| Flag / variable | Meaning |
|---|---|
| `-tz` / `GOOGLE_HEALTH_TZ` | Time zone for "today" (default: your computer's) |
| `-extra-scopes` / `GOOGLE_HEALTH_EXTRA_SCOPES` | Extra read scopes to request at `login` |
| `-no-browser` | `login` prints the URL instead of opening a browser |
| `GOOGLE_HEALTH_MCP_DIR` | Where `credentials.json` and the token are kept |
| `logout` | Deletes the saved token |

Both servers also read `GOOGLE_HEALTH_API_BASE`, which replaces the Health API
base URL.

**Limits.** `get_data_points` returns at most about 200 records (about 120 KB)
and says when it stopped. Dense data such as heart rate belongs in `get_rollup`
or `get_daily_totals`. The API's maximum date ranges (90 days, or 14 for some
types) are handled by splitting requests.

---

## Development

```sh
go test -race ./...      # unit tests, against a fake API
go run . serve           # local stdio server
```

| Path | What's there |
|---|---|
| `main.go` | Commands: `login`, `status`, `logout`, `serve`, `serve-http` |
| `internal/remote` | Hosted mode: OAuth server for MCP clients, consent page, Google sign-in, Streamable HTTP |
| `internal/auth` | Local mode: browser login with PKCE, token file |
| `internal/health` | Google Health API client, data-type catalog, date parsing |
| `internal/tools` | The MCP tools |
| `internal/testapi` | Fake API for tests |
| `deploy/`, `Dockerfile` | Cloud Run deploy script, CI bootstrap, container image |

Issues and pull requests are welcome. Security problems: see [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE)
