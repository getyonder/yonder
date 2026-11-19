#!/bin/sh
# Builds Yonder for Linux, Windows and macOS into dist/, all from Linux.
# Needs Go, zip and tar; for the Linux build also GTK3 and webkit2gtk-4.1
# with their headers. Windows and macOS need no C compiler: Wails drives
# them without cgo (the `purego` tag on macOS).
#
# With YONDER_SIGN=1 the two Mac apps are signed with an Apple Developer ID
# and notarized before they are packed (packaging/sign-macos.sh says which
# files it needs). Without it nothing is signed.
set -eu
cd "$(dirname "$0")/.."
sign=${YONDER_SIGN:-}
if [ "$sign" = 1 ] && ! command -v "${RCODESIGN:-rcodesign}" >/dev/null; then
	echo "YONDER_SIGN=1 needs rcodesign (set RCODESIGN, or put it on the PATH)" >&2
	exit 1
fi
version=$(sed -n 's/^const version = "\(.*\)"$/\1/p' cmd/yonder/main.go)
# The version that Windows shows in the properties of Yonder.exe is in a
# file kept in the repo, which must be made again with each new version.
if ! grep -a -q "name=\"app.yonder.desktop\" version=\"$version.0\"" cmd/yonder/rsrc_windows_amd64.syso; then
	echo "cmd/yonder/rsrc_windows_amd64.syso is not of version $version: run packaging/syso.sh and commit the file, then build" >&2
	exit 1
fi
rm -rf dist
mkdir -p dist
# production is Wails's tag for a build that goes to people: without it the
# window has developer tools and a right-click menu with Reload.
flags="-trimpath"

# with_licences puts beside the app the licence text of what it is built
# from: the fonts (inside the program too, with them) and the Go modules.
packaging/licences.sh > dist/THIRD-PARTY-LICENCES.txt
with_licences() {
	mkdir -p "$1"
	cp cmd/yonder/frontend/fonts/*-OFL.txt dist/THIRD-PARTY-LICENCES.txt "$1"
}

echo "Linux (amd64)"
out=dist/yonder-$version-linux-amd64
mkdir -p "$out"
go build $flags -tags "gtk3 production" -ldflags "-s -w" -o "$out/yonder" ./cmd/yonder
cp packaging/linux/yonder.desktop packaging/linux/install.sh "$out"
cp packaging/icon.png "$out/yonder.png"
with_licences "$out/licences"
tar -C dist -czf "$out.tar.gz" "$(basename "$out")"

echo "Windows (amd64)"
out=dist/yonder-$version-windows-amd64
mkdir -p "$out"
# windowsgui: no console window behind the app.
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build $flags -tags production -ldflags "-s -w -H windowsgui" -o "$out/Yonder.exe" ./cmd/yonder
with_licences "$out/licences"
(cd dist && zip -qr "$(basename "$out").zip" "$(basename "$out")")

for arch in arm64 amd64; do
	echo "macOS ($arch)"
	out=dist/yonder-$version-macos-$arch
	app=$out/Yonder.app/Contents
	mkdir -p "$app/MacOS" "$app/Resources"
	GOOS=darwin GOARCH=$arch CGO_ENABLED=0 go build $flags -tags "purego production" -ldflags "-s -w" -o "$app/MacOS/yonder" ./cmd/yonder
	sed "s/VERSION/$version/g" packaging/macos/Info.plist > "$app/Info.plist"
	cp packaging/macos/icon.icns "$app/Resources/icon.icns"
	with_licences "$app/Resources/licences"
	if [ "$sign" = 1 ]; then
		packaging/sign-macos.sh "$out/Yonder.app"
	fi
	(cd dist && zip -qr "$(basename "$out").zip" "$(basename "$out")")
done

# SHA256SUMS lists the checksum of each package, so a download can be
# checked with `sha256sum -c SHA256SUMS`; publish it beside the packages.
(cd dist && sha256sum *.tar.gz *.zip > SHA256SUMS)

ls -lh dist/*.tar.gz dist/*.zip dist/SHA256SUMS
