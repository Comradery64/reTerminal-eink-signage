#!/usr/bin/env bash
# Set up keyless Workload Identity Federation so the broker's k8s ServiceAccount can mint a
# short-lived Google SA token — NO service-account key (org policy blocks key create+upload).
#
# Because the k3s OIDC issuer is on-prem / not publicly reachable, we register the provider with the
# cluster's UPLOADED JWKS (--jwk-json-path) rather than the discovery/issuer-URL flow. The k8s SA
# presents a projected token (audience = the WIF provider); STS exchanges it for an impersonated,
# 1-hour SA access token scoped to calendar.freebusy. Nothing long-lived is ever at rest.
#
# Safe by default: prints the plan (DRY-RUN) and changes nothing. Pass --apply to execute.
# Creates are idempotent (existing pool/provider/binding are detected and left as-is). The final
# step rewrites POOL_ID/PROVIDER_ID placeholders in broker.yaml (both the projected-token audience
# AND the broker-gcp-credconfig ConfigMap).
#
# Prereqs (the operator runs this — needs cluster + GCP admin):
#   - gcloud authed as a principal with workloadIdentityPools admin + serviceAccounts.setIamPolicy
#   - kubectl pointed at the target k3s cluster (to read its issuer + JWKS)
#   - jq on PATH
#
# Usage:
#   ./setup_wif.sh                 # discover issuer/JWKS + show the plan (DRY-RUN)
#   ./setup_wif.sh --apply         # create pool/provider/binding + patch broker.yaml
#
# All values below are placeholders — override via env with your own project/SA:
#   PROJECT_ID, PROJECT_NUM, SA, POOL_ID, PROVIDER_ID, K8S_NAMESPACE, K8S_SA, BROKER_YAML
set -euo pipefail

PROJECT_ID="${PROJECT_ID:-your-gcp-project-id}"
PROJECT_NUM="${PROJECT_NUM:-000000000000}"
SA="${SA:-your-broker-sa@your-gcp-project-id.iam.gserviceaccount.com}"
POOL_ID="${POOL_ID:-displays-pool}"
PROVIDER_ID="${PROVIDER_ID:-k3s-oidc}"   # GCP requires 4–32 chars; "k3s" alone is rejected
K8S_NAMESPACE="${K8S_NAMESPACE:-meeting-displays}"
K8S_SA="${K8S_SA:-default}"

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BROKER_YAML="${BROKER_YAML:-$here/../backend/deploy/k3s/broker.yaml}"

APPLY=0; [ "${1:-}" = "--apply" ] && APPLY=1

# The projected k8s token's `sub` is system:serviceaccount:<ns>:<sa>; that's both the WIF subject
# and the IAM member we authorize to impersonate the GCP SA.
SUBJECT="system:serviceaccount:${K8S_NAMESPACE}:${K8S_SA}"
POOL_RES="projects/${PROJECT_NUM}/locations/global/workloadIdentityPools/${POOL_ID}"
AUDIENCE="//iam.googleapis.com/${POOL_RES}/providers/${PROVIDER_ID}"
MEMBER="principal://iam.googleapis.com/${POOL_RES}/subject/${SUBJECT}"

for bin in gcloud kubectl jq; do
  command -v "$bin" >/dev/null || { echo "ERROR: '$bin' not found on PATH."; exit 1; }
done
[ -f "$BROKER_YAML" ] || { echo "ERROR: broker.yaml not found at $BROKER_YAML"; exit 1; }

echo "Project:    $PROJECT_ID ($PROJECT_NUM)"
echo "SA:         $SA"
echo "Pool/Prov:  $POOL_ID / $PROVIDER_ID"
echo "K8s member: $SUBJECT"
echo "Audience:   $AUDIENCE"
echo "broker.yaml:$BROKER_YAML"
echo "Mode:       $([ $APPLY -eq 1 ] && echo APPLY || echo 'DRY-RUN (use --apply to execute)')"
echo "──────────────────────────────────────────────────────────────────────────────"

# ── Step 1: read the cluster's OIDC issuer + JWKS (read-only; safe in dry-run) ────────────────
jwks="$(mktemp)"; trap 'rm -f "$jwks"' EXIT
echo "→ Reading cluster OIDC issuer + JWKS via kubectl…"
ISSUER="$(kubectl get --raw /.well-known/openid-configuration | jq -r .issuer)"
[ -n "$ISSUER" ] && [ "$ISSUER" != "null" ] || { echo "ERROR: could not read issuer from /.well-known/openid-configuration"; exit 1; }
kubectl get --raw /openid/v1/jwks > "$jwks"
nkeys="$(jq '.keys | length' < "$jwks")"
echo "  issuer: $ISSUER"
echo "  JWKS:   $nkeys key(s) uploaded inline (on-prem issuer → no public discovery)"
[ "$nkeys" -ge 1 ] || { echo "ERROR: JWKS has no keys"; exit 1; }

# Small gcloud helper: run on --apply, otherwise just print the command.
run() { if [ $APPLY -eq 1 ]; then "$@"; else printf '   would run:'; printf ' %q' "$@"; echo; fi; }

# ── Step 2: workload-identity pool (idempotent) ───────────────────────────────────────────────
echo "→ Workload Identity pool: $POOL_ID"
if gcloud iam workload-identity-pools describe "$POOL_ID" \
     --project="$PROJECT_ID" --location=global >/dev/null 2>&1; then
  echo "  already exists — leaving as-is."
else
  run gcloud iam workload-identity-pools create "$POOL_ID" \
    --project="$PROJECT_ID" --location=global --display-name="Meeting Displays"
fi

# ── Step 3: OIDC provider with UPLOADED JWKS (idempotent) ─────────────────────────────────────
echo "→ OIDC provider: $PROVIDER_ID (uploaded-JWKS variant)"
if gcloud iam workload-identity-pools providers describe "$PROVIDER_ID" \
     --project="$PROJECT_ID" --location=global --workload-identity-pool="$POOL_ID" >/dev/null 2>&1; then
  echo "  already exists — leaving as-is. (To rotate JWKS: providers update-oidc --jwk-json-path …)"
else
  run gcloud iam workload-identity-pools providers create-oidc "$PROVIDER_ID" \
    --project="$PROJECT_ID" --location=global --workload-identity-pool="$POOL_ID" \
    --issuer-uri="$ISSUER" --jwk-json-path="$jwks" \
    --attribute-mapping="google.subject=assertion.sub" \
    --allowed-audiences="$AUDIENCE"
fi

# ── Step 4: let the k8s SA impersonate the GCP SA (idempotent — add-iam-policy-binding) ────────
echo "→ IAM binding: roles/iam.workloadIdentityUser for $SUBJECT"
run gcloud iam service-accounts add-iam-policy-binding "$SA" --project="$PROJECT_ID" \
  --role=roles/iam.workloadIdentityUser --member="$MEMBER"

# ── Step 5: patch POOL_ID/PROVIDER_ID placeholders in broker.yaml ──────────────────────────────
echo "→ Patching broker.yaml (projected-token audience + broker-gcp-credconfig ConfigMap)"
if grep -q 'workloadIdentityPools/POOL_ID/providers/PROVIDER_ID' "$BROKER_YAML"; then
  if [ $APPLY -eq 1 ]; then
    tmp="$(mktemp)"
    sed -e "s#workloadIdentityPools/POOL_ID/providers/PROVIDER_ID#workloadIdentityPools/${POOL_ID}/providers/${PROVIDER_ID}#g" \
      "$BROKER_YAML" > "$tmp" && mv "$tmp" "$BROKER_YAML"
    n="$(grep -c "workloadIdentityPools/${POOL_ID}/providers/${PROVIDER_ID}" "$BROKER_YAML")"
    echo "  patched $n occurrence(s) → $POOL_ID / $PROVIDER_ID"
  else
    echo "   would replace POOL_ID→$POOL_ID and PROVIDER_ID→$PROVIDER_ID in $(grep -c 'workloadIdentityPools/POOL_ID/providers/PROVIDER_ID' "$BROKER_YAML") line(s)"
  fi
else
  echo "  no POOL_ID/PROVIDER_ID placeholders found — already patched (or custom values), skipping."
fi

echo "──────────────────────────────────────────────────────────────────────────────"
if [ $APPLY -eq 1 ]; then
  cat <<EOF
WIF configured. Next (docs/DEPLOY.md Parts D2–D4 / F):
  1) kubectl create namespace $K8S_NAMESPACE   # if not present
  2) kubectl -n $K8S_NAMESPACE create secret generic broker-secrets \\
       --from-literal=alert-webhook-url='https://hooks.slack.com/...'   # optional Slack
  3) Set image: in broker.yaml (Part C), paste provisioning/rooms.snippet.yaml into the ConfigMap.
  4) kubectl apply -f $(basename "$BROKER_YAML") -f alerts.yaml ; then watch the broker logs (Part F).
EOF
else
  echo "Nothing changed (dry-run). Re-run with --apply to create the pool/provider/binding and patch broker.yaml."
fi
