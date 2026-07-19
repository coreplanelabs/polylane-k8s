#!/usr/bin/env bash
set -euo pipefail

git fetch --prune --unshallow 2>/dev/null || true
git fetch --tags --force

source bin/activate-hermit
source .buildkite/steps/lib/gh-app-token.sh

goreleaser release --clean
