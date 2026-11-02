#!/bin/sh
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
BACKEND_DIR="$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)"
REPO_DIR="$(CDPATH= cd -- "$BACKEND_DIR/.." && pwd)"
VERSION_FILE="$BACKEND_DIR/cmd/server/VERSION"

# Only an exact codeq release tag may override this fork's VERSION file.
# Upstream tags can share the checkout and must not relabel a codeq build.
if command -v git >/dev/null 2>&1; then
  TAG="$(
    git -C "$REPO_DIR" tag --points-at HEAD --list 'v[0-9]*-codeq' '[0-9]*-codeq' 2>/dev/null |
      grep -E '^v?[0-9]+\.[0-9]+\.[0-9]+-codeq$' |
      head -n 1 || true
  )"
  if [ -n "$TAG" ]; then
    printf '%s\n' "${TAG#v}"
    exit 0
  fi
fi

printf '%s\n' "$(tr -d '\r\n' < "$VERSION_FILE")"
