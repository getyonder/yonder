#!/bin/sh
# Writes, to standard output, the licence text of everything compiled into
# Yonder that is not ours: every module any build uses (Linux, Windows, macOS
# and the web app). Go's own licence is the one its x/sys module carries.
# These licences ask that their text travel with the programs, so build.sh
# and api.sh put this beside each one, and website.sh on the website. A module with no licence file stops
# the build. The fonts' licences are here too, as well as beside the fonts.
set -eu
cd "$(dirname "$0")/.."
tab=$(printf '\t')

# one prints a heading and the licence files found in a folder.
one() {
	found=
	for f in "$2"/*; do
		[ -f "$f" ] || continue
		case "$(basename "$f" | tr '[:lower:]' '[:upper:]')" in
		*.GO) continue ;;
		LICENSE* | LICENCE* | COPYING* | UNLICENSE* | NOTICE* | PATENTS* | *-OFL.TXT) ;;
		*) continue ;;
		esac
		[ -n "$found" ] || printf '\n%s\n%s\n%s\n\n' "================================================================" "$1" "================================================================"
		found=1
		cat "$f"
		echo
	done
	[ -n "$found" ] || { echo "licences.sh: no licence file for $1 in $2" >&2; exit 1; }
}

echo "Yonder is licensed under the Apache License, Version 2.0."
echo
echo "It includes the software and fonts listed below. Each part is followed by"
echo "its licence, as its authors published it."
echo "Go itself, whose runtime and standard library are in every build, is under"
echo "the licence printed below for golang.org/x/sys, by the same authors."
one "Bricolage Grotesque and Figtree (fonts)" cmd/yonder/frontend/fonts
for target in linux/amd64 windows/amd64 darwin/arm64 darwin/amd64 js/wasm; do
	GOOS=${target%/*} GOARCH=${target#*/} go list -deps -tags "gtk3 purego production" \
		-f '{{with .Module}}{{if not .Main}}{{.Path}}'"$tab"'{{.Version}}'"$tab"'{{.Dir}}{{end}}{{end}}' ./cmd/...
done | sort -u | while IFS=$tab read -r path version dir; do
	one "$path $version" "$dir"
done
