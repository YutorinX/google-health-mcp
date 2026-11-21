#!/usr/bin/env bash
# Deploy google-health-mcp to Google Cloud Run in your own project.
#
#   PROJECT_ID=my-project ./deploy/cloudrun.sh url
#       prints the service URL and the redirect URI to put on your Google
#       OAuth "Web application" client (run this before creating the client)
#
#   PROJECT_ID=my-project \
#   ALLOWED_EMAILS=you@gmail.com \     # or '*' for any Google account (public mode)
#   GOOGLE_OAUTH_CLIENT_ID=123-abc.apps.googleusercontent.com \
#   GOOGLE_OAUTH_CLIENT_SECRET=GOCSPX-... \
#   ./deploy/cloudrun.sh deploy
#       enables APIs, stores secrets in Secret Manager, builds and deploys.
#       Re-run it to ship code changes (the client secret can be omitted
#       once stored).
#
# Optional: REGION (default europe-north1), SERVICE (default
# google-health-mcp), GOOGLE_HEALTH_TZ (default UTC; e.g. Europe/Stockholm),
# GOOGLE_HEALTH_EXTRA_SCOPES (e.g. nutrition.readonly,ecg.readonly).
set -euo pipefail

: "${PROJECT_ID:?set PROJECT_ID to your Google Cloud project id}"
REGION="${REGION:-europe-north1}"
SERVICE="${SERVICE:-google-health-mcp}"
TZ_NAME="${GOOGLE_HEALTH_TZ:-UTC}"
EXTRA_SCOPES="${GOOGLE_HEALTH_EXTRA_SCOPES:-}"
KEY_SECRET="${SERVICE}-signing-key"
CLIENT_SECRET_NAME="${SERVICE}-google-client-secret"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

g() { gcloud --project "$PROJECT_ID" --quiet "$@"; }

PROJECT_NUMBER="$(g projects describe "$PROJECT_ID" --format='value(projectNumber)')"
URL="https://${SERVICE}-${PROJECT_NUMBER}.${REGION}.run.app"
RUN_SA="${SERVICE}-run@${PROJECT_ID}.iam.gserviceaccount.com"
BUILD_SA="${PROJECT_NUMBER}-compute@developer.gserviceaccount.com"

cmd="${1:-deploy}"
case "$cmd" in
url)
  cat <<EOF
Service URL:            $URL
MCP URL (for agents):   $URL/mcp
OAuth redirect URI:     $URL/oauth/google/callback
EOF
  exit 0
  ;;
deploy) ;;
*)
  echo "usage: $0 [url|deploy]" >&2
  exit 2
  ;;
esac

: "${ALLOWED_EMAILS:?set ALLOWED_EMAILS to the Google account(s) allowed to sign in, comma-separated}"
: "${GOOGLE_OAUTH_CLIENT_ID:?set GOOGLE_OAUTH_CLIENT_ID (Web application client)}"

echo "==> Enabling APIs (first run takes a minute)"
g services enable run.googleapis.com cloudbuild.googleapis.com artifactregistry.googleapis.com \
  secretmanager.googleapis.com
g services enable health.googleapis.com ||
  echo "    warning: couldn't enable health.googleapis.com here; enable \"Google Health API\" in the console's API Library"

echo "==> Secrets"
if ! g secrets describe "$KEY_SECRET" >/dev/null 2>&1; then
  g secrets create "$KEY_SECRET" --replication-policy=automatic
  openssl rand -base64 32 | tr -d '\n' | g secrets versions add "$KEY_SECRET" --data-file=-
  echo "    created token-encryption key"
fi
if ! g secrets describe "$CLIENT_SECRET_NAME" >/dev/null 2>&1; then
  : "${GOOGLE_OAUTH_CLIENT_SECRET:?set GOOGLE_OAUTH_CLIENT_SECRET for the first deploy}"
  g secrets create "$CLIENT_SECRET_NAME" --replication-policy=automatic
fi
if [[ -n "${GOOGLE_OAUTH_CLIENT_SECRET:-}" ]]; then
  printf %s "$GOOGLE_OAUTH_CLIENT_SECRET" | g secrets versions add "$CLIENT_SECRET_NAME" --data-file=- >/dev/null
  echo "    stored Google OAuth client secret"
fi

echo "==> Runtime service account (can read only its two secrets)"
if ! g iam service-accounts describe "$RUN_SA" >/dev/null 2>&1; then
  g iam service-accounts create "${SERVICE}-run" --display-name="google-health-mcp runtime"
  sleep 10 # new service accounts take a moment to become usable in IAM bindings
fi
for s in "$KEY_SECRET" "$CLIENT_SECRET_NAME"; do
  g secrets add-iam-policy-binding "$s" --member="serviceAccount:$RUN_SA" \
    --role=roles/secretmanager.secretAccessor >/dev/null
done

echo "==> Letting Cloud Build build from source"
g projects add-iam-policy-binding "$PROJECT_ID" --member="serviceAccount:$BUILD_SA" \
  --role=roles/run.builder --condition=None >/dev/null ||
  echo "    warning: could not grant roles/run.builder to $BUILD_SA; if the build fails with a permission error, grant it manually"

echo "==> Building and deploying $SERVICE to $REGION"
# ^|^ switches the env-var separator so commas in values survive.
g run deploy "$SERVICE" \
  --region "$REGION" \
  --source "$ROOT" \
  --service-account "$RUN_SA" \
  --allow-unauthenticated \
  --min-instances 0 --max-instances 2 \
  --cpu 1 --memory 256Mi --concurrency 20 --timeout 120 \
  --set-env-vars "^|^PUBLIC_URL=$URL|ALLOWED_EMAILS=$ALLOWED_EMAILS|GOOGLE_OAUTH_CLIENT_ID=$GOOGLE_OAUTH_CLIENT_ID|GOOGLE_HEALTH_TZ=$TZ_NAME|GOOGLE_HEALTH_EXTRA_SCOPES=$EXTRA_SCOPES" \
  --set-secrets "MCP_SIGNING_KEY=${KEY_SECRET}:latest,GOOGLE_OAUTH_CLIENT_SECRET=${CLIENT_SECRET_NAME}:latest"

if curl -fsS "$URL/health" >/dev/null 2>&1; then
  echo "==> Health check OK"
else
  echo "==> warning: $URL/health did not answer yet; check: gcloud run services logs read $SERVICE --region $REGION --project $PROJECT_ID"
fi

cat <<EOF

Done. Add this as a custom connector / remote MCP server in your agent:
  $URL/mcp
then connect and sign in with Google. Open $URL for instructions.
EOF
