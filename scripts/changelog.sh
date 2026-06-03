#!/usr/bin/env bash
# Generate release notes since a previous tag.
# Usage: ./scripts/changelog.sh v0.1.0 [0.1.1]
# Writes markdown to stdout.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

PREV="${1:?usage: $0 <previous-tag> [new-version]}"
NEW="${2:-}"

PREV_TAG="${PREV#v}"
PREV_TAG="v${PREV_TAG}"

if [[ -n "$NEW" ]]; then
  NEW_TAG="${NEW#v}"
  NEW_TAG="v${NEW_TAG}"
else
  NEW_TAG="$(git describe --tags --abbrev=0 HEAD 2>/dev/null || true)"
  if [[ -z "$NEW_TAG" ]]; then
    echo "error: could not determine new version; pass it as the second argument" >&2
    exit 1
  fi
fi

REPO="${GITHUB_REPOSITORY:-yufei-pan/maiGoLLMRouter}"
COMPARE_URL="https://github.com/${REPO}/compare/${PREV_TAG}...${NEW_TAG}"

body=""
if command -v gh >/dev/null 2>&1; then
  if notes="$(gh api "repos/${REPO}/releases/generate-notes" \
    -f "tag_name=${NEW_TAG}" \
    -f "target_commitish=HEAD" \
    -f "previous_tag_name=${PREV_TAG}" \
    --jq .body 2>/dev/null)"; then
    # API often returns only the compare link before the tag exists.
    if [[ "$notes" == *"What's Changed"* ]] || [[ "$notes" == *$'\n'-* ]]; then
      body="$notes"
    fi
  fi
fi

if [[ -z "$body" ]]; then
  commits="$(git log "${PREV_TAG}..HEAD" --pretty=format:'- %s (%h)' --no-merges --reverse 2>/dev/null || true)"
  if [[ -z "$commits" ]]; then
    commits="- (no commits since ${PREV_TAG})"
  fi
  body="## What's Changed

${commits}"
fi

printf '%s\n\n' "$body"
if [[ "$body" != *"${COMPARE_URL}"* ]]; then
  echo "**Full Changelog**: ${COMPARE_URL}"
fi
