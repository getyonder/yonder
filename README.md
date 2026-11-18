# Yonder

Send files between your devices, on the same network or not. Pair two devices once, then send from anywhere: no account, no cloud storage. On the same network, Yonder also sends to and receives from [LocalSend](https://localsend.org) as it is.

Website and web app: <https://getyonder.org>

## How it works

- Between networks, files go directly from one device to the other, encrypted the whole way. Two apps use a tunnel made with [tailcat](https://github.com/tailscale/tailcat); a transfer with a browser uses a WebRTC data channel. A relay helps the two devices find a direct path and carries no file: if the networks allow no direct path, the send stops and says so.
- On the same network, devices find each other and exchange files with the LocalSend protocol.
- Pairing is done once per pair of devices, with a link, a QR code or a six-digit code.

The whole design, with figures, is in [docs/design.md](docs/design.md) (also as a [PDF](docs/design.pdf)).

## Layout

- `cmd/yonder`: the desktop app (a [Wails](https://wails.io) window; also usable from the terminal, see the top of `cmd/yonder/main.go`)
- `cmd/yonder-web`: the web app at <https://app.getyonder.org/>: Yonder as WebAssembly, with the desktop window's interface (`cmd/yonder/frontend`) and its own bridge to the browser (`cmd/yonder-web/page`)
- `cmd/yonder-api`: the service at `api.getyonder.org`: the meeting point where devices find each other, the list of relays and the update check. It never sees a file
- `cmd/yonder-stun`: the STUN server, which tells a device its public address
- `cmd/yonder-withdraw`: signs the list of withdrawn versions that the update check carries
- `internal/`: the tunnel, pairing, the LocalSend protocol and the rest
- `packaging/`: build scripts, icons and installers

The website and the settings of Yonder's own servers are not in this repository.

## Building

Needs Go. On Linux the window also needs GTK 4 and WebKitGTK 6.0 with their headers.

```sh
go build ./cmd/yonder        # the desktop app
go test ./...                # tests; YONDER_NET_TESTS=1 adds those that use the relay
packaging/build.sh           # packages for Linux, Windows and macOS, into dist/
GOOS=js GOARCH=wasm go build -o yonder.wasm ./cmd/yonder-web   # the web app's program
```

A plain `go build` gives a window that has the web view's developer tools. The packages are built with the `production` tag, which leaves them out.

## Contact

Questions and faults: <hello@getyonder.org>, or an issue in this repository. How Yonder treats your data: <https://getyonder.org/privacy/>

## Licence

Apache-2.0; see [LICENSE](LICENSE) and [NOTICE](NOTICE). Yonder speaks the LocalSend protocol and is not affiliated with LocalSend.
