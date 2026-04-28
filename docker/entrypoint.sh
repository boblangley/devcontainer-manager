#!/usr/bin/env sh
set -eu

export MUTAGEN_DATA_DIRECTORY="${MUTAGEN_DATA_DIRECTORY:-/var/lib/devcontainer-manager/mutagen}"
mkdir -p "$MUTAGEN_DATA_DIRECTORY"

if command -v mutagen >/dev/null 2>&1; then
  mutagen daemon start || true
fi

exec devcontainer-manager "$@"

