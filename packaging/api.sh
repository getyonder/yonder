#!/bin/sh
# Builds the release of the service the apps and the web app call into
# dist/api: the list of relays and the update check. Needs only Go. Serve
# it with cmd/yonder-api, which is also the meeting point. The web app is
# no part of it (see webapp.sh), and nor is the website (see website.sh).
set -eu
cd "$(dirname "$0")/.."
api=dist/api
rm -rf "$api"
mkdir -p "$api"
cp cmd/yonder-api/files/relays.json "$api"
# The newest version, which the desktop app asks for once a day to say when a
# newer one is out. So a release with a new version number goes out only
# once that version's packages can be downloaded from the page named here.
version=$(sed -n 's/^const version = "\(.*\)"$/\1/p' cmd/yonder/main.go)
# The file also carries the signed list of withdrawn versions,
# packaging/withdrawn.json, which yonder-withdraw makes. The release stops
# here if the list's signature does not agree with the key in the app.
go run ./cmd/yonder-withdraw -latest "$version" > "$api/latest.json"
ls -l "$api"
