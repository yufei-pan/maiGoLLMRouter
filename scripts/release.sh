#!/usr/bin/env bash
# Build release archives for maiGoLLMRouter.
# Usage: ./scripts/release.sh 0.1.0
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

VERSION="${1:?usage: $0 <version> (e.g. 0.1.0)}"
VERSION="${VERSION#v}"
DIST="$ROOT/dist"
STAGING="$DIST/staging"
NAME="maiGoLLMRouter"

LDFLAGS="-s -w -X main.version=${VERSION}"
export CGO_ENABLED=0

rm -rf "$DIST"
mkdir -p "$STAGING"

platforms=(
  "linux:amd64:tar.gz"
  "linux:arm64:tar.gz"
  "darwin:amd64:tar.gz"
  "darwin:arm64:tar.gz"
  "windows:amd64:zip"
)

for spec in "${platforms[@]}"; do
  IFS=: read -r goos goarch ext <<<"$spec"
  pkg="${NAME}-${VERSION}-${goos}-${goarch}"
  dir="$STAGING/$pkg"
  mkdir -p "$dir"

  bin="$NAME"
  if [[ "$goos" == windows ]]; then
    bin="${NAME}.exe"
  fi

  echo "==> ${goos}/${goarch}"
  GOOS="$goos" GOARCH="$goarch" go build -ldflags "$LDFLAGS" -o "$dir/$bin" .

  cp README.md config.example.toml "$dir/"

  archive="$DIST/${pkg}.${ext}"
  rm -f "$archive"
  if [[ "$ext" == zip ]]; then
    (cd "$STAGING" && zip -rq "$archive" "$pkg")
  else
    tar -C "$STAGING" -czf "$archive" "$pkg"
  fi
done

echo "==> source tarball"
src_pkg="${NAME}-${VERSION}-src"
src_archive="$DIST/${src_pkg}.tar.gz"
git archive --format=tar.gz --prefix="${src_pkg}/" -o "$src_archive" HEAD

rm -rf "$STAGING"

echo "==> checksums"
(
  cd "$DIST"
  shasum -a 256 ./*.tar.gz ./*.zip 2>/dev/null | sed 's|  \./|  |' > SHA256SUMS.txt
)

echo "Built $DIST:"
ls -la "$DIST"
