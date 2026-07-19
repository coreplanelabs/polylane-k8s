#!/usr/bin/env bash
set -euo pipefail

git fetch --prune --unshallow 2>/dev/null || true
git fetch --tags --force

source bin/activate-hermit
source .buildkite/steps/lib/gh-app-token.sh

npx --yes release-please@16 release-pr \
  --token="$GITHUB_TOKEN" \
  --repo-url=coreplanelabs/polylane-k8s \
  --target-branch="$BUILDKITE_BRANCH"

release_output="$(mktemp)"
trap 'rm -f "$release_output"' EXIT
if ! GITHUB_OUTPUT="$release_output" npx --yes release-please@16 github-release \
  --token="$GITHUB_TOKEN" \
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

printf '%s' "$GITHUB_TOKEN" |
  helm registry login ghcr.io --username coreplanelabs --password-stdin
helm package charts/polylane-k8s --destination dist/
helm push "dist/polylane-k8s-${version}.tgz" oci://ghcr.io/coreplanelabs/charts
