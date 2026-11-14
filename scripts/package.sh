#!/usr/bin/env bash
# Build the release archives into dist/: one per operating system and architecture, plus checksums.txt.
#
#   scripts/package.sh v0.1.0
#
# Linux and macOS get emx_<version>_<os>_<arch>.tar.gz, Windows gets a .zip holding emx.exe.
# Each archive also carries LICENSE, NOTICE and README.md. Binaries are static (no cgo).
set -euo pipefail

tag="${1:?usage: scripts/package.sh <tag, e.g. v0.1.0>}"
version="${tag#v}"
cd "$(dirname "$0")/.."
rm -rf dist
mkdir -p dist

for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do
  goos="${target%/*}"
  goarch="${target#*/}"
  stage="$(mktemp -d)"
  bin=emx
  [ "$goos" = windows ] && bin=emx.exe
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -trimpath \
    -ldflags "-s -w -X github.com/LottoLottoLotto/endpointmaxxing/internal/cli.Version=${tag}" \
    -o "$stage/$bin" ./cmd/emx
  cp LICENSE NOTICE README.md "$stage/"
  name="emx_${version}_${goos}_${goarch}"
  if [ "$goos" = windows ]; then
    (cd "$stage" && zip -q "$OLDPWD/dist/$name.zip" "$bin" LICENSE NOTICE README.md)
  else
    tar -C "$stage" -czf "dist/$name.tar.gz" "$bin" LICENSE NOTICE README.md
  fi
  rm -rf "$stage"
  echo "built $name"
done

(cd dist && sha256sum emx_* > checksums.txt)
