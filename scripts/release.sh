#!/usr/bin/env bash
# 构建多平台发布包：二进制 + README/LICENSE/SCHEMA/INTEGRATION + docs。
# 用法：VERSION=v1.0.0 ./scripts/release.sh
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="${VERSION:-${1:-}}"
if [ -z "$VERSION" ]; then
  echo "用法：VERSION=v1.0.0 $0" >&2
  exit 2
fi
case "$VERSION" in
  v*) ;;
  *) echo "VERSION 必须以 v 开头，例如 v1.0.0" >&2; exit 2 ;;
esac

for f in README.md LICENSE THIRD_PARTY_LICENSES.md SCHEMA.md INTEGRATION.md; do
  [ -f "$f" ] || { echo "缺少 $f" >&2; exit 1; }
done
for tool in tar zip; do
  command -v "$tool" >/dev/null || { echo "缺少 $tool" >&2; exit 1; }
done

DIST="${DIST:-dist}"
COMMIT="${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo unknown)}"
BUILD_DATE="${BUILD_DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
LDFLAGS="-s -w -X main.version=$VERSION -X main.commit=$COMMIT -X main.buildDate=$BUILD_DATE"

echo "== tests =="
test -z "$(gofmt -l .)" || { echo "gofmt 未通过" >&2; exit 1; }
go vet ./...
go test ./...

rm -rf "$DIST"
mkdir -p "$DIST"

targets=(darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64)
for target in "${targets[@]}"; do
  goos="${target%/*}"
  goarch="${target#*/}"
  name="brainx_${VERSION}_${goos}_${goarch}"
  stage="$DIST/$name"
  mkdir -p "$stage"
  bin="brainx"
  [ "$goos" = "windows" ] && bin="brainx.exe"
  echo "== build $name =="
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    go build -trimpath -ldflags "$LDFLAGS" -o "$stage/$bin" ./cmd/brainx
  cp README.md LICENSE THIRD_PARTY_LICENSES.md SCHEMA.md INTEGRATION.md "$stage/"
  cp -R licenses "$stage/"
  if [ "$goos" = "windows" ]; then
    (cd "$DIST" && zip -q -r "$name.zip" "$name")
  else
    tar -czf "$DIST/$name.tar.gz" -C "$DIST" "$name"
  fi
  rm -rf "$stage"
done

cd "$DIST"
if command -v sha256sum >/dev/null 2>&1; then
  sha256sum ./* > checksums.txt
else
  shasum -a 256 ./* > checksums.txt
fi
printf '\n== artifacts ==\n'
ls -lh
printf '\n== checksums ==\n'
cat checksums.txt
printf '\nversion=%s commit=%s built=%s\n' "$VERSION" "$COMMIT" "$BUILD_DATE"
