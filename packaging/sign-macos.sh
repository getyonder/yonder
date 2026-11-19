#!/bin/sh
# Signs one Yonder.app with an Apple Developer ID, has Apple's notary
# service accept it, and staples the notary's ticket to it. From Linux,
# with rcodesign (the project apple-codesign); no Mac is needed.
#
#   packaging/sign-macos.sh dist/yonder-VERSION-macos-arm64/Yonder.app
#
# packaging/build.sh calls it for both Mac apps when YONDER_SIGN=1. The
# keys are files that the caller names:
#   YONDER_P12                the Developer ID Application certificate with
#                             its key, as one .p12 file
#   YONDER_P12_PASSWORD_FILE  a file with that .p12 file's password and
#                             nothing more (no line end)
#   YONDER_NOTARY_KEY         the App Store Connect API key as the JSON file
#                             that `rcodesign encode-app-store-connect-api-key`
#                             makes
#   RCODESIGN                 the rcodesign program, if it is not on the PATH
# rcodesign 0.29.0 opens a .p12 file only in the older encoding
# (openssl pkcs12 -export -certpbe PBE-SHA1-3DES -keypbe PBE-SHA1-3DES
# -macalg sha1).
set -eu
[ $# -eq 1 ] && [ -d "$1" ] || { echo "usage: packaging/sign-macos.sh FOLDER/Yonder.app" >&2; exit 2; }
app=$1
rcodesign=${RCODESIGN:-rcodesign}
command -v "$rcodesign" >/dev/null || { echo "sign-macos.sh: rcodesign is not found (set RCODESIGN)" >&2; exit 1; }
for name in YONDER_P12 YONDER_P12_PASSWORD_FILE YONDER_NOTARY_KEY; do
	eval "file=\${$name:-}"
	[ -r "$file" ] || { echo "sign-macos.sh: $name names no file that can be read" >&2; exit 1; }
done

# --for-notarization sets the hardened runtime and a timestamp, which the
# notary service needs.
"$rcodesign" sign --p12-file "$YONDER_P12" --p12-password-file "$YONDER_P12_PASSWORD_FILE" --for-notarization "$app"

# The notary service takes a zip, and the ticket goes onto the app.
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
(cd "$(dirname "$app")" && zip -qry "$tmp/app.zip" "$(basename "$app")")
"$rcodesign" notary-submit --api-key-file "$YONDER_NOTARY_KEY" --wait "$tmp/app.zip"
"$rcodesign" staple "$app"
