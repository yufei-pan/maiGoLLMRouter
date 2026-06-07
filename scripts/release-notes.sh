#!/usr/bin/env bash
# Build full GitHub release notes (changelog + download table).
# Usage: ./scripts/release-notes.sh 0.1.8 v0.1.7 > dist/RELEASE_NOTES.md
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

VERSION="${1:?usage: $0 <version> [previous-tag]}"
PREV="${2:-}"

VERSION="${VERSION#v}"
NAME="maiGoLLMRouter"

if [[ -z "$PREV" ]]; then
  PREV="$(git tag -l 'v*' --sort=-v:refname | grep -v "^v${VERSION}$" | head -1 || true)"
fi
if [[ -z "$PREV" ]]; then
  echo "error: no previous tag found; pass it as the second argument" >&2
  exit 1
fi

{
  echo "# v${VERSION}"
  echo ""
  "$ROOT/scripts/changelog.sh" "$PREV" "$VERSION"
  echo ""
  echo "## Downloads"
  echo ""
  echo "| Archive | Platform |"
  echo "|---------|----------|"
  echo "| \`${NAME}-${VERSION}-linux-amd64.tar.gz\` | Linux x86_64 |"
  echo "| \`${NAME}-${VERSION}-linux-arm64.tar.gz\` | Linux ARM64 |"
  echo "| \`${NAME}-${VERSION}-darwin-amd64.tar.gz\` | macOS Intel |"
  echo "| \`${NAME}-${VERSION}-darwin-arm64.tar.gz\` | macOS Apple Silicon |"
  echo "| \`${NAME}-${VERSION}-windows-amd64.zip\` | Windows x86_64 |"
  echo "| \`${NAME}-${VERSION}-src.tar.gz\` | Source |"
  echo ""
  echo "Verify checksums: \`sha256sum -c SHA256SUMS.txt\`"
  echo ""
  echo "Each binary archive includes \`README.md\` and \`config.example.toml\`."
} 
