#!/bin/sh
# Runs this package's tests in a browser, where pion is the browser's own
# WebRTC: `go test` for js/wasm has only Node, which has none. From the
# repo root:
#
#   internal/direct/testdata/browser/run.sh [arguments of the test program]
#
# for example `run.sh -test.count=5 -test.run=TLS`. It needs chromium. The
# two ends of each test are in one page, so nothing leaves this machine.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
trap 'kill $browser 2>/dev/null || true; rm -rf "$work"' EXIT

GOOS=js GOARCH=wasm go test -c -o "$work/www/direct.test.wasm" ./internal/direct
cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" "$here/index.html" "$work/www"
go build -o "$work/serve" "$here/serve.go"

query=
for arg in "$@"; do query="$query&arg=$arg"; done
"$work/serve" "$work/www" > "$work/result" &
server=$!
sleep 1 # the server binds; the browser asks again if it is early
chromium --headless=new --no-first-run --user-data-dir="$work/profile" "http://127.0.0.1:8467/?${query#&}" >/dev/null 2>&1 &
browser=$!
wait $server || true
cat "$work/result"
grep -q '^EXIT 0$' "$work/result"
