#!/bin/sh
# Makes cmd/yonder/rsrc_windows_amd64.syso, which puts the icon, the version
# and the manifest into Yonder.exe: `go build` picks a .syso up by its name.
# Run it after changing the version in cmd/yonder/main.go, and commit the
# result with it: build.sh refuses to build when the two do not agree,
# because Windows shows the version of this file in the program's
# properties. Needs Go, to run the Wails tools.
set -eu
cd "$(dirname "$0")/.."
version=$(sed -n 's/^const version = "\(.*\)"$/\1/p' cmd/yonder/main.go)
wails="go run github.com/wailsapp/wails/v3/cmd/wails3@v3.1.0"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

$wails generate icons -input packaging/icon.png -windowsfilename "$tmp/icon.ico" -macfilename "$tmp/icon.icns"
sed "s/VERSION/$version/g" packaging/windows/info.json > "$tmp/info.json"
sed "s/VERSION/$version/g" packaging/windows/yonder.exe.manifest > "$tmp/yonder.exe.manifest"
$wails generate syso -arch amd64 -icon "$tmp/icon.ico" -manifest "$tmp/yonder.exe.manifest" -info "$tmp/info.json" -out cmd/yonder/rsrc_windows_amd64.syso
