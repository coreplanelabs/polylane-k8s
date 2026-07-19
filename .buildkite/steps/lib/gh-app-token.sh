#!/usr/bin/env bash
set -euo pipefail

if ! command -v op >/dev/null 2>&1; then
  echo "op CLI is required on PATH" >&2
  return 1
fi
: "${OP_SERVICE_ACCOUNT_TOKEN:?OP_SERVICE_ACCOUNT_TOKEN must be set}"

client_id="$(op read "op://CI/coreplane-bot/client-id")"
private_key="$(op read "op://CI/coreplane-bot/private-key")"

base64url() {
  base64 | tr '+/' '-_' | tr -d '=\n'
}

now="$(date +%s)"
jwt_header="$(printf '%s' '{"alg":"RS256","typ":"JWT"}' | base64url)"
jwt_payload="$(jq -cn \
  --argjson iat "$((now - 60))" \
  --argjson exp "$((now + 540))" \
  --arg iss "$client_id" \
  '{iat: $iat, exp: $exp, iss: $iss}' | base64url)"
jwt_unsigned="${jwt_header}.${jwt_payload}"
jwt_signature="$(
  printf '%s' "$jwt_unsigned" |
    openssl dgst -sha256 -sign <(printf '%s' "$private_key") |
    base64url
)"
app_jwt="${jwt_unsigned}.${jwt_signature}"

github_headers=(
  -H "Accept: application/vnd.github+json"
  -H "Authorization: Bearer ${app_jwt}"
  -H "X-GitHub-Api-Version: 2022-11-28"
)

if ! installation_response="$(
  curl --fail-with-body --silent --show-error \
    "${github_headers[@]}" \
    "https://api.github.com/repos/coreplanelabs/polylane-k8s/installation"
)"; then
  echo "failed to get GitHub App installation" >&2
  return 1
fi
installation_id="$(printf '%s' "$installation_response" | jq -er '.id')"

if ! token_response="$(
  curl --fail-with-body --silent --show-error \
    -X POST \
    "${github_headers[@]}" \
    "https://api.github.com/app/installations/${installation_id}/access_tokens"
)"; then
  echo "failed to create GitHub App installation token" >&2
  return 1
fi
export GITHUB_TOKEN="$(printf '%s' "$token_response" | jq -er '.token')"
