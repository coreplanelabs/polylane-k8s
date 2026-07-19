#!/usr/bin/env bash
set -euo pipefail

export XDG_CACHE_HOME="$PWD/.cache"
export GOMODCACHE="$PWD/.cache/go-mod"
mkdir -p "$XDG_CACHE_HOME" "$GOMODCACHE"
source bin/activate-hermit

task do && task test:full && git diff --exit-code
