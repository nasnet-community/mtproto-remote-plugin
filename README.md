# mtproto-remote-plugin

Go packages and commands for running Telegram MTProto proxy servers
([telemt](https://github.com/telemt/telemt)) on remote Linux hosts, with their
upstream connections carried through a panel instead of leaving the host.

## What is here

| Path | What it is |
|---|---|
| `cmd/mtproto-agent` | the small agent that runs on the remote host: supervises telemt, captures its Telegram connections with nft and carries them to the panel |
| `mux`, `reality`, `realityserver`, `link`, `ctrl`, `tgnet` | the transport: a stream multiplexer over a TLS 1.3 connection that the panel dials, the control protocol on top, and the address allowlist |
| `hubnode`, `relayops`, `installer` | the panel side: the control loop for one agent, and install / repair / update / rotate / uninstall over SSH and the link |
| `telemt`, `release`, `layout` | telemt config rendering, pinned release assets, file locations on the host |

## Develop

```sh
make test    # format, vet, go test -race
make lab     # the same tests on Linux, in Docker
make drill   # root-only drills in network namespaces, in Docker
```

## Licence

MIT, except `reality/client.go`, a port of xray-core code that stays under
MPL-2.0 (see `reality/LICENSE`).
