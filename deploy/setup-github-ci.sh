#!/usr/bin/env bash
# One-time setup so GitHub Actions can deploy to Cloud Run without any
# stored keys, using Workload Identity Federation.
#
#   PROJECT_ID=my-project GITHUB_REPO=you/google-health-mcp ./deploy/setup-github-ci.sh
#
# Run it after the first `deploy/cloudrun.sh deploy` (which creates the
# service, its runtime service account and secrets). It creates:
#   - an Artifact Registry repo for images
#   - a deployer service account that can only push images, deploy new
#     revisions and act as the runtime service account
#   - a workload identity pool + provider that trusts GitHub's OIDC tokens
#     only from GITHUB_REPO on refs/heads/main
# and sets the repository variables the workflow reads (via `gh` if it is
# installed and logged in; otherwise it prints them).
set -euo pipefail

: "${PROJECT_ID:?set PROJECT_ID to your Google Cloud project id}"
: "${GITHUB_REPO:?set GITHUB_REPO to owner/name of the GitHub repository}"
REGION="${REGION:-europe-north1}"
SERVICE="${SERVICE:-google-health-mcp}"
POOL="${SERVICE:0:24}-gh"
PROVIDER="github"

g() { gcloud --project "$PROJECT_ID" --quiet "$@"; }

PROJECT_NUMBER="$(g projects describe "$PROJECT_ID" --format='value(projectNumber)')"
RUN_SA="${SERVICE}-run@${PROJECT_ID}.iam.gserviceaccount.com"
DEPLOY_SA_NAME="${SERVICE:0:21}-deployer"
DEPLOY_SA="${DEPLOY_SA_NAME}@${PROJECT_ID}.iam.gserviceaccount.com"
PROVIDER_NAME="projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/${POOL}/providers/${PROVIDER}"

if ! g iam service-accounts describe "$RUN_SA" >/dev/null 2>&1; then
  echo "Runtime service account $RUN_SA not found. Run deploy/cloudrun.sh deploy first." >&2
  exit 1
fi

echo "==> Enabling APIs"
g services enable iam.googleapis.com iamcredentials.googleapis.com sts.googleapis.com \
  artifactregistry.googleapis.com run.googleapis.com

echo "==> Artifact Registry repository ${SERVICE} in ${REGION}"
if ! g artifacts repositories describe "$SERVICE" --location "$REGION" >/dev/null 2>&1; then
  g artifacts repositories create "$SERVICE" --location "$REGION" --repository-format docker \
    --description "google-health-mcp images (pushed by GitHub Actions)"
fi

echo "==> Deployer service account ${DEPLOY_SA}"
if ! g iam service-accounts describe "$DEPLOY_SA" >/dev/null 2>&1; then
  g iam service-accounts create "$DEPLOY_SA_NAME" --display-name "google-health-mcp GitHub deployer"
  sleep 10 # let the new account propagate before binding roles to it
fi
g artifacts repositories add-iam-policy-binding "$SERVICE" --location "$REGION" \
  --member "serviceAccount:$DEPLOY_SA" --role roles/artifactregistry.writer >/dev/null
g projects add-iam-policy-binding "$PROJECT_ID" \
  --member "serviceAccount:$DEPLOY_SA" --role roles/run.developer --condition=None >/dev/null
g iam service-accounts add-iam-policy-binding "$RUN_SA" \
  --member "serviceAccount:$DEPLOY_SA" --role roles/iam.serviceAccountUser >/dev/null

echo "==> Workload identity pool ${POOL}"
if ! g iam workload-identity-pools describe "$POOL" --location global >/dev/null 2>&1; then
  g iam workload-identity-pools create "$POOL" --location global --display-name "GitHub Actions"
fi
CONDITION="assertion.repository == '${GITHUB_REPO}' && assertion.ref == 'refs/heads/main'"
if ! g iam workload-identity-pools providers describe "$PROVIDER" --workload-identity-pool "$POOL" --location global >/dev/null 2>&1; then
  g iam workload-identity-pools providers create-oidc "$PROVIDER" \
    --workload-identity-pool "$POOL" --location global \
    --display-name "GitHub ${GITHUB_REPO}" \
    --issuer-uri "https://token.actions.githubusercontent.com" \
    --attribute-mapping "google.subject=assertion.sub,attribute.repository=assertion.repository,attribute.ref=assertion.ref" \
    --attribute-condition "$CONDITION"
else
  g iam workload-identity-pools providers update-oidc "$PROVIDER" \
    --workload-identity-pool "$POOL" --location global --attribute-condition "$CONDITION" >/dev/null
fi
g iam service-accounts add-iam-policy-binding "$DEPLOY_SA" \
  --role roles/iam.workloadIdentityUser \
  --member "principalSet://iam.googleapis.com/projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/${POOL}/attribute.repository/${GITHUB_REPO}" >/dev/null

# Plain "NAME=value" pairs rather than an associative array: macOS ships bash 3.2.
VARS=(
  "GCP_PROJECT_ID=$PROJECT_ID"
  "GCP_REGION=$REGION"
  "CLOUD_RUN_SERVICE=$SERVICE"
  "GCP_WIF_PROVIDER=$PROVIDER_NAME"
  "GCP_DEPLOYER_SA=$DEPLOY_SA"
)

echo "==> GitHub repository variables"
if command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
  for kv in "${VARS[@]}"; do
    gh variable set "${kv%%=*}" --repo "$GITHUB_REPO" --body "${kv#*=}"
  done
  echo "    set on ${GITHUB_REPO}"
else
  echo "    gh isn't available; add these under Settings → Secrets and variables → Actions → Variables:"
  for kv in "${VARS[@]}"; do
    printf '    %-18s %s\n' "${kv%%=*}" "${kv#*=}"
  done
fi

cat <<EOF

Done. Every push to main in ${GITHUB_REPO} now tests, builds and deploys.
Trigger the first run with:  gh workflow run ci.yml --repo ${GITHUB_REPO}
EOF
