#!/bin/sh
# Lays a public repository's files into a folder, from the last commit here.
# Nothing else from this repository goes. The folder becomes a git repository
# if it is not one; what was in it before is replaced. It commits and pushes
# nothing.
#
#	packaging/publish.sh FOLDER           Yonder: the app, the web app and the service, and the files in public/ at the top
#	packaging/publish.sh omarchy FOLDER   the Omarchy plugin: omarchy/ at the top (the marketplace
#	                                      wants manifest.json there), with Yonder's licence text
set -eu
# What stays private: the usage page's program with the package only it
# uses, and the website's build script. The website (website/) and the
# servers' settings (server/) are at the top and are never copied. The
# service (cmd/yonder-api), the STUN server and the tool that signs the
# withdrawn list are public since 0.1.41, the first public version.
private="cmd/yonder-stats internal/stats packaging/website.sh"
what=yonder
[ "${1:-}" = omarchy ] && { what=omarchy; shift; }
[ $# -eq 1 ] || { echo "usage: packaging/publish.sh [omarchy] FOLDER" >&2; exit 2; }
cd "$(dirname "$0")/.."
dest=$1
mkdir -p "$dest"
[ -d "$dest/.git" ] || git init -q -b main "$dest"
git -C "$dest" rm -rqf --ignore-unmatch .
if [ $what = omarchy ]; then
	git archive HEAD:omarchy | tar -x -C "$dest"
	git show HEAD:public/LICENSE >"$dest/LICENSE"
	echo "__pycache__/" >"$dest/.gitignore"
else
	git archive HEAD cmd internal packaging go.mod go.sum | tar -x -C "$dest"
	git archive HEAD:public | tar -x -C "$dest"
	for path in $private; do
		[ -e "$dest/$path" ] || { echo "publish.sh: $path is not there; correct the list" >&2; exit 1; }
		rm -r "$dest/$path"
	done
	# go.mod names only what the public programs use. The copy must stand
	# alone: code that needs what stays private stops here.
	(cd "$dest" && go mod tidy && go vet -tags gtk3 ./... && GOOS=js GOARCH=wasm go vet ./cmd/yonder-web)
fi
git -C "$dest" add -A
git -C "$dest" status --short | awk '{n[substr($1,1,1)]++} END {printf "added %d, changed %d, removed %d\n", n["A"], n["M"], n["D"]}'
