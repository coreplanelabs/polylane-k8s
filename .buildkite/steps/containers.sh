#!/usr/bin/env bash
# Multi-arch build (and, off PRs, push) of the polylane-k8s agent image.
# Buildkite port of the docker/metadata-action + build-push-action steps in
# .github/workflows/containers.yaml.
#
# Deviations from the GitHub workflow, all forced by the platform:
#   - Tag/label derivation is done here in shell instead of via
#     docker/metadata-action.
#   - Layer caching uses the Namespace hosted-agent containerd cache
#     (automatic) instead of GitHub Actions' type=gha cache.
#   - The push credential is a Buildkite secret (GHCR_TOKEN) rather than
#     the Actions-provided GITHUB_TOKEN.
set -euo pipefail

IMAGE="ghcr.io/coreplanelabs/polylane-k8s"

# --- event classification (mirrors the metadata-action event keys) --------
is_pr=false
[ "${BUILDKITE_PULL_REQUEST:-false}" != "false" ] && is_pr=true

tag_ref="${BUILDKITE_TAG:-}"
branch_ref="${BUILDKITE_BRANCH:-}"
# metadata-action slugifies refs (slashes -> dashes) for use as image tags.
branch_slug="$(printf '%s' "$branch_ref" | tr '/' '-')"
short_sha="$(git rev-parse --short HEAD)"
commit_time="$(git show -s --format=%cI HEAD)"

# --- assemble --tag flags --------------------------------------------------
tags=()
version=""
if [ -n "$tag_ref" ]; then
  # type=semver on a v* tag: X.Y.Z and X.Y.
  semver="${tag_ref#v}"
  version="$semver"
  tags+=("--tag" "${IMAGE}:${semver}")
  majmin="$(printf '%s' "$semver" | cut -d. -f1-2)"
  tags+=("--tag" "${IMAGE}:${majmin}")
elif [ "$is_pr" = true ]; then
  # type=ref,event=pr
  tags+=("--tag" "${IMAGE}:pr-${BUILDKITE_PULL_REQUEST}")
else
  # type=ref,event=branch (+ -{{sha}} suffix), and latest on the default branch.
  tags+=("--tag" "${IMAGE}:${branch_slug}")
  tags+=("--tag" "${IMAGE}:${branch_slug}-${short_sha}")
  if [ "$branch_ref" = "main" ]; then
    tags+=("--tag" "${IMAGE}:latest")
  fi
fi

# --- OCI labels (the two metadata-action set explicitly, plus provenance) --
labels=(
  "--label" "org.opencontainers.image.title=polylane-k8s"
  "--label" "org.opencontainers.image.description=Minimal read-only Kubernetes agent connecting clusters to Polylane via Cloudflare Tunnel"
  "--label" "org.opencontainers.image.revision=$(git rev-parse HEAD)"
  "--label" "org.opencontainers.image.source=https://github.com/coreplanelabs/polylane-k8s"
  "--label" "org.opencontainers.image.created=${commit_time}"
)
[ -n "$version" ] && labels+=("--label" "org.opencontainers.image.version=${version}")

# --- push vs. build-only ---------------------------------------------------
# The hosted agents' default buildx builder is a remote BuildKit that
# supports multi-platform and pushes straight to the registry, so (unlike
# the e2e job) it is left as-is here.
output=()
if [ "$is_pr" = true ]; then
  echo "--- Pull request: building both arches for validation, not pushing"
else
  echo "--- Logging in to GHCR"
  GHCR_TOKEN="$(buildkite-agent secret get GHCR_TOKEN)"
  printf '%s' "$GHCR_TOKEN" | docker login ghcr.io -u coreplanelabs --password-stdin
  output=("--push")
fi

set -x
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  --file containers/polylane-k8s.Dockerfile \
  "${tags[@]}" \
  "${labels[@]}" \
  --build-arg "GIT_COMMIT=${short_sha}" \
  --build-arg "BUILD_TIME=${commit_time}" \
  --build-arg "VERSION=${version}" \
  "${output[@]}" \
  .
