#!/bin/sh
# Builds the web app into dist/webapp: the desktop window's interface, with
# the web bridge and stylesheet in place of the window's, and Yonder itself
# as WebAssembly. They are plain files, and any server of files can host
# them (nginx does: server/nginx.conf). The page calls the service the apps
# call (internal/address), or the one the file service.json beside it names
# (cmd/yonder-web). Needs only Go. The service is no part of it: see api.sh.
set -eu
cd "$(dirname "$0")/.."
out=dist/webapp
rm -rf "$out"
mkdir -p "$out"
cp -r cmd/yonder/frontend/. "$out"
cp cmd/yonder-web/page/* "$out"
cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" "$out"
# The licences of what Yonder is built from travel with the web app.
packaging/licences.sh > "$out/THIRD-PARTY-LICENCES.txt"
version=$(sed -n 's/^const version = "\(.*\)"$/\1/p' cmd/yonder/main.go)
# The page asks for service.json at each start. The release's one names no
# service, so the page calls the built-in one; without the file the answer
# is a 404, which a browser's console shows as an error.
echo '{}' > "$out/service.json"
# The icon a browser asks for by itself; they take a PNG under this name.
cp cmd/yonder/icons/tray.png "$out/favicon.ico"
GOOS=js GOARCH=wasm go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$out/yonder.wasm" ./cmd/yonder-web
# The program is large and compresses to about a quarter; the server sends
# this copy to browsers that take gzip, which is all of them.
gzip -9 -k "$out/yonder.wasm"
ls -l "$out"/yonder.wasm* | awk '{printf "%s  %.1f MB\n", $NF, $5/1048576}'
