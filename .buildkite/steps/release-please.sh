#!/usr/bin/env bash
set -euo pipefail

client_id="$(buildkite-agent secret get COREPLANE_BOT_CLIENT_ID)"
private_key="$(buildkite-agent secret get COREPLANE_BOT_PRIVATE_SIGNING_KEY)"

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
  exit 1
fi
installation_id="$(printf '%s' "$installation_response" | jq -er '.id')"

if ! token_response="$(
  curl --fail-with-body --silent --show-error \
    -X POST \
    "${github_headers[@]}" \
    "https://api.github.com/app/installations/${installation_id}/access_tokens"
)"; then
  echo "failed to create GitHub App installation token" >&2
  exit 1
fi
token="$(printf '%s' "$token_response" | jq -er '.token')"

npx --yes release-please@16 release-pr \
  --token="$token" \
  --repo-url=coreplanelabs/polylane-k8s \
  --target-branch="$BUILDKITE_BRANCH"

release_output="$(mktemp)"
trap 'rm -f "$release_output"' EXIT
if ! GITHUB_OUTPUT="$release_output" npx --yes release-please@16 github-release \
  --token="$token" \
  --repo-url=coreplanelabs/polylane-k8s \
  --target-branch="$BUILDKITE_BRANCH"; then
  echo "release-please github-release failed" >&2
  exit 1
fi

if [ ! -s "$release_output" ]; then
  echo "release-please created no release"
  exit 0
fi

release_created="$(awk -F= '$1 == "release_created" { print $2; exit }' "$release_output")"
if [ "$release_created" != "true" ]; then
  echo "release-please created no release"
  exit 0
fi

tag_name="$(awk -F= '$1 == "tag_name" { print $2; exit }' "$release_output")"
version="$(awk -F= '$1 == "version" { print $2; exit }' "$release_output")"
if [ -z "$tag_name" ] || [ -z "$version" ]; then
  echo "release-please output omitted tag_name or version" >&2
  exit 1
fi

git fetch --tags --force
git checkout "$tag_name"

source bin/activate-hermit
printf '%s' "$(buildkite-agent secret get GHCR_TOKEN)" |
  helm registry login ghcr.io --username coreplanelabs --password-stdin
helm package charts/polylane-k8s --destination dist/
helm push "dist/polylane-k8s-${version}.tgz" oci://ghcr.io/coreplanelabs/charts
