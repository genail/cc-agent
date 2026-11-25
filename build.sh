#!/bin/sh
# Builds release binaries and checksums into dist/.
set -eu

version="${1:?usage: build.sh <version>}"
rm -rf dist
mkdir dist
for target in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64; do
	os="${target%/*}"
	arch="${target#*/}"
	CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "-s -w -X main.version=$version" \
		-o "dist/cc-agent_${os}_${arch}" .
done
cd dist
if command -v sha256sum >/dev/null; then sha256sum cc-agent_* >SHA256SUMS; else shasum -a 256 cc-agent_* >SHA256SUMS; fi
