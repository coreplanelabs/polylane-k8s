#!/usr/bin/env bash
set -euo pipefail

git fetch --prune --unshallow 2>/dev/null || true
git fetch --tags --force

source bin/activate-hermit
export GITHUB_TOKEN="$(buildkite-agent secret get GITHUB_TOKEN)"

goreleaser release --clean
