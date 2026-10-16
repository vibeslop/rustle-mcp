#!/bin/sh
# Installs rustle-mcp and asks which of Claude Code, Codex and Cursor to add it to:
#
#   curl -fsSL https://raw.githubusercontent.com/vibeslop/rustle-mcp/main/install.sh | sh
#
# Run it again to update. Arguments go to `rustle-mcp setup`: `sh -s -- claude` adds it to
# Claude Code without asking. RUSTLE_INSTALL_DIR picks the folder (default ~/.local/bin);
# RUSTLE_VERSION a release (default the latest).
set -eu

repo=vibeslop/rustle-mcp
dir=${RUSTLE_INSTALL_DIR:-"$HOME/.local/bin"}

fail() {
  printf 'rustle-mcp install: %s\n' "$*" >&2
  exit 1
}

case $(uname -s) in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) fail "no prebuilt binary for $(uname -s); see https://github.com/$repo#install" ;;
esac
case $(uname -m) in
  arm64 | aarch64) arch=arm64 ;;
  x86_64 | amd64) arch=amd64 ;;
  *) fail "no prebuilt binary for $(uname -m); see https://github.com/$repo#install" ;;
esac

if [ -n "${RUSTLE_VERSION:-}" ]; then
  base="https://github.com/$repo/releases/download/v${RUSTLE_VERSION#v}"
else
  base="https://github.com/$repo/releases/latest/download"
fi
name="rustle-mcp_${os}_${arch}"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
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
printf 'Installed rustle-mcp %s to %s\n' "$("$dir/rustle-mcp" --version)" "$dir/rustle-mcp"

if ! command -v ffmpeg >/dev/null 2>&1 && [ ! -x /opt/homebrew/bin/ffmpeg ] && [ ! -x /usr/local/bin/ffmpeg ]; then
  echo "Frames and sound need ffmpeg (brew install ffmpeg, or sudo apt install ffmpeg); transcripts and search work without it."
fi
"$dir/rustle-mcp" setup "$@"
