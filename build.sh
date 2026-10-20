#!/bin/sh
# build.sh — builds the plugin into one .tar to install from a Chriscord
# server's admin panel (Plugins → Install from a file).
#
#   ./build.sh                 every platform in plugin.json
#   ./build.sh linux-amd64     just these (linux-amd64 linux-arm64 macos-amd64 macos-arm64)
#
# → dist/chriscord-music-plugin-<version>.tar, holding plugin.json, the
#   programs in bin/, README.md and LICENSE. Needs Go 1.24+ and tar.
set -eu
cd "$(dirname "$0")"

version=$(sed -n 's/^ *"version": *"\([^"]*\)".*/\1/p' plugin.json | head -n 1)
[ -n "$version" ] || { echo "no version in plugin.json" >&2; exit 1; }
targets=${*:-linux-amd64 linux-arm64 macos-amd64 macos-arm64}

stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
mkdir -p "$stage/bin" dist

for t in $targets; do
    os=${t%-*}; arch=${t#*-}
    goos=$os; [ "$os" = macos ] && goos=darwin
    echo "building $t…"
    CGO_ENABLED=0 GOOS=$goos GOARCH=$arch go build -trimpath -ldflags "-s -w -X main.version=$version" \
        -o "$stage/bin/music-$t" .
done

cp plugin.json README.md "$stage/"
[ -f LICENSE ] && cp LICENSE "$stage/"
[ -f pfp.png ] && cp pfp.png "$stage/"   # a default picture, if you add one
# Its own icon in the app, if you add one: icon.ico (or icon.png), named in
# plugin.json as "icon". Without it the app shows its profile picture.
for f in icon.ico icon.png; do if [ -f "$f" ]; then cp "$f" "$stage/"; fi; done

out="dist/chriscord-music-plugin-$version.tar"
# Plain ustar, owned by nobody in particular: the server's reader takes
# files and folders only (no links).
tar --format=ustar --owner=0 --group=0 --numeric-owner -C "$stage" -cf "$out" $(cd "$stage" && ls -A)
echo "→ $out ($(du -h "$out" | cut -f1))"
