#!/bin/sh
# Redraws every icon from the two SVGs here. Run it after changing an SVG
# (after changing only the version, syso.sh is enough); the results are
# kept in the repo, so building needs none of this. Needs rsvg-convert, and
# Go to run the Wails icon tools.
set -eu
cd "$(dirname "$0")/.."
wails="go run github.com/wailsapp/wails/v3/cmd/wails3@v3.1.0"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

rsvg-convert -w 512 -h 512 packaging/icon.svg -o packaging/icon.png
rsvg-convert -w 64 -h 64 packaging/icon.svg -o cmd/yonder/icons/tray.png
rsvg-convert packaging/tray-template.svg -o cmd/yonder/icons/tray-template.png

# The macOS icon, and the Windows .syso (the icon, the version and the
# manifest inside Yonder.exe).
$wails generate icons -input packaging/icon.png -windowsfilename "$tmp/icon.ico" -macfilename packaging/macos/icon.icns
packaging/syso.sh
