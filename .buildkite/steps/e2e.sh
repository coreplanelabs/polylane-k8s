#!/usr/bin/env bash
set -euo pipefail

export XDG_CACHE_HOME="$PWD/.cache"
export GOMODCACHE="$PWD/.cache/go-mod"
mkdir -p "$XDG_CACHE_HOME" "$GOMODCACHE"
source bin/activate-hermit

# Hosted agents ship a working Docker daemon, which kind needs. They
# also default the active buildx builder to a remote one whose output
# only lands in the build cache, so a plain `docker build -t x` (as the
# e2e harness runs) never reaches the local image store and `kind load
# docker-image` can't find it. Selecting the `default` (docker-driver)
# builder makes builds load into the daemon like they do locally.
docker buildx use default
task e2e
