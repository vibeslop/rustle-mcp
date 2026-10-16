#!/bin/sh
# Installs the latest rustle-mcp release for this machine, then runs `rustle-mcp setup`
# with any arguments given:
#
#   curl -fsSL https://raw.githubusercontent.com/vibeslop/rustle-mcp/main/install.sh | sh -s -- claude
#
# RUSTLE_INSTALL_DIR picks the folder (default ~/.local/bin); RUSTLE_VERSION a release (default latest).
set -eu

repo=vibeslop/rustle-mcp
dir=${RUSTLE_INSTALL_DIR:-"$HOME/.local/bin"}

say() { printf '%s\n' "$*" >&2; }
fail() { say "rustle-mcp install: $*"; exit 1; }

case $(uname -s) in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) fail "no prebuilt binary for $(uname -s); download one from https://github.com/$repo/releases or run: go install github.com/$repo@latest" ;;
esac
case $(uname -m) in
  arm64 | aarch64) arch=arm64 ;;
  x86_64 | amd64) arch=amd64 ;;
  *) fail "no prebuilt binary for $(uname -m); run: go install github.com/$repo@latest" ;;
esac

if [ -n "${RUSTLE_VERSION:-}" ]; then
  tag="v${RUSTLE_VERSION#v}"
else
  # The latest release's page redirects to its tag.
  tag=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$repo/releases/latest") || fail "could not reach GitHub"
  tag=${tag##*/}
  case $tag in v*) ;; *) fail "found no release" ;; esac
fi
version=${tag#v}
name="rustle-mcp_${version}_${os}_${arch}"
base="https://github.com/$repo/releases/download/$tag"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
say "Downloading rustle-mcp $version for $os/$arch"
curl -fsSL -o "$work/$name.tar.gz" "$base/$name.tar.gz" || fail "could not download $base/$name.tar.gz"
curl -fsSL -o "$work/checksums.txt" "$base/checksums.txt" || fail "could not download the checksums"

expected=$(grep " $name.tar.gz\$" "$work/checksums.txt" | cut -d' ' -f1)
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$work/$name.tar.gz" | cut -d' ' -f1)
else
  actual=$(shasum -a 256 "$work/$name.tar.gz" | cut -d' ' -f1)
fi
[ -n "$expected" ] && [ "$expected" = "$actual" ] || fail "the download does not match its checksum"

tar -xzf "$work/$name.tar.gz" -C "$work"
mkdir -p "$dir"
# Replace the file, not its contents: a running server keeps the binary it started with.
cp "$work/$name/rustle-mcp" "$dir/rustle-mcp.new"
chmod 755 "$dir/rustle-mcp.new"
mv -f "$dir/rustle-mcp.new" "$dir/rustle-mcp"
say "Installed $dir/rustle-mcp"

if ! command -v ffmpeg >/dev/null 2>&1; then
  say "Frames and sound need ffmpeg: brew install ffmpeg, or sudo apt install ffmpeg. Transcripts and search work without it."
fi
if [ $# -gt 0 ]; then
  exec "$dir/rustle-mcp" setup "$@"
fi
say "Next: $dir/rustle-mcp setup claude (or codex, cursor, json)"
