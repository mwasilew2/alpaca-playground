#!/usr/bin/env bash
# One-time SigNoz first-run setup.
#
# SigNoz's collector (ingester) is OpAMP-managed: it will NOT open its OTLP
# receiver (:4317/:4318) until an organization exists in the backend. Until then
# every OTLP export fails silently ("cannot create agent without orgId" in the
# backend logs). This script creates the initial admin/org via the API so the
# collector comes up. Idempotent: it no-ops if setup is already complete.
#
# Dev-only default credentials — override via env for anything shared.
set -euo pipefail

UI="${SIGNOZ_UI:-http://localhost:8080}"
EMAIL="${SIGNOZ_EMAIL:-admin@alpaca.local}"
PASSWORD="${SIGNOZ_PASSWORD:-Alpaca12345!}"
ORG="${SIGNOZ_ORG:-alpaca}"

echo "==> waiting for SigNoz UI at ${UI} ..."
for _ in $(seq 1 60); do
  curl -sf -m 3 "${UI}/api/v1/version" >/dev/null 2>&1 && break
  sleep 2
done

if curl -s -m 5 "${UI}/api/v1/version" | grep -q '"setupCompleted":true'; then
  echo "==> SigNoz already set up. UI: ${UI}"
  exit 0
fi

echo "==> creating first admin/org ..."
curl -sf -m 10 -X POST "${UI}/api/v1/register" \
  -H 'Content-Type: application/json' \
  -d "{\"name\":\"admin\",\"orgName\":\"${ORG}\",\"email\":\"${EMAIL}\",\"password\":\"${PASSWORD}\"}" \
  >/dev/null

echo "==> done. UI: ${UI}   login: ${EMAIL} / ${PASSWORD}"
echo "    The collector will open its OTLP receiver (:4317/:4318) within ~30s."
