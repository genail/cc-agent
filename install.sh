#!/bin/sh
# Installs the latest cc-agent release into ~/.local/bin (or $CC_AGENT_BIN_DIR).
set -eu

repo="genail/cc-agent"
bin_dir="${CC_AGENT_BIN_DIR:-$HOME/.local/bin}"

fail() {
	echo "cc-agent: $1" >&2
	exit 1
}

case "$(uname -s)" in
Darwin) os=darwin ;;
Linux) os=linux ;;
*) fail "unsupported system $(uname -s); on Windows use WSL" ;;
esac
case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
arm64 | aarch64) arch=arm64 ;;
*) fail "unsupported architecture $(uname -m)" ;;
esac

file="cc-agent_${os}_${arch}"
base="${CC_AGENT_BASE_URL:-https://github.com/$repo/releases/latest/download}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

curl -fsSL -o "$tmp/$file" "$base/$file" || fail "download failed: $base/$file"
curl -fsSL -o "$tmp/SHA256SUMS" "$base/SHA256SUMS" || fail "download failed: $base/SHA256SUMS"

if command -v sha256sum >/dev/null; then
	actual="$(sha256sum "$tmp/$file" | cut -d' ' -f1)"
else
	actual="$(shasum -a 256 "$tmp/$file" | cut -d' ' -f1)"
fi
grep -qx "$actual  $file" "$tmp/SHA256SUMS" || fail "checksum mismatch"

mkdir -p "$bin_dir"
chmod 755 "$tmp/$file"
mv -f "$tmp/$file" "$bin_dir/cc-agent"
echo "Installed $bin_dir/cc-agent ($("$bin_dir/cc-agent" version))"

case ":$PATH:" in
*":$bin_dir:"*) ;;
*) echo "Add $bin_dir to your PATH, then restart your terminal and Claude Code." ;;
esac
