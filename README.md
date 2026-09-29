# mtproto-remote-plugin

A NASNET Panel plugin that installs an MTProto proxy on **your own server** over SSH.
It runs as a small container on the MikroTik router. You enter a server address and SSH
login in its web page. It then downloads the proxy from GitHub and installs it on the
server as a systemd service.

By default it installs [9seconds/mtg](https://github.com/9seconds/mtg). Any GitHub
project that publishes Linux release binaries works too (see [Settings](#settings)).

The image is one static binary on `scratch`, about 3.3 MB.

## How a deployment works

1. **Host key check.** The app reads the server's SSH host key and shows its SHA256
   fingerprint. You confirm it on the first connection. Later connections go ahead only
   if the key has not changed. If it has changed, the page shows a warning and waits
   for an explicit confirmation.
2. **Login.** It logs in with a password. If the server has turned off plain password
   login, it tries keyboard-interactive login with the same password. As a non-root
   user, every root step runs through `sudo` with the same password.
3. **Server check.** It reads `uname -m` and requires `systemctl`.
4. **Download.** The router downloads the release asset for the server's architecture
   from GitHub. If the asset is a `.tar.gz`, the router unpacks it. The server never
   needs access to GitHub.
5. **Install.** The router uploads the binary to `/usr/local/bin/<binary>`. It writes
   `/etc/systemd/system/<service>.service`, which runs the binary as a `DynamicUser`
   with only `CAP_NET_BIND_SERVICE`. Then it enables and restarts the service.
6. **Verify.** After 3 seconds it checks `systemctl is-active` and shows the
   `systemctl status` output. It also gives a `t.me/proxy` link to share.

To update, deploy again: this replaces the binary and the unit, then restarts the
service.

The password is kept only in memory while the deployment runs. It is never written to
disk or to a log. The only thing stored on disk is the list of confirmed host keys, in
`$DATA_DIR/known_hosts.json`.

## Settings

The panel passes these to the container as environment variables. A blank value uses
the default.

| Variable        | Default                              | Meaning                                                                         |
| --------------- | ------------------------------------ | ------------------------------------------------------------------------------- |
| `GITHUB_REPO`   | `9seconds/mtg`                       | Repository whose releases are installed                                         |
| `RELEASE_TAG`   | *(latest)*                           | Pin a release tag, e.g. `v2.2.8`                                                |
| `ASSET_PATTERN` | `mtg-*-linux-{arch}.tar.gz`          | Glob over asset names. `{arch}` becomes `amd64`, `arm64`, `armv7`, `armv6`, `386`, `mips` or `mipsle` |
| `BINARY_NAME`   | `mtg`                                | File to take from an archive; installed as `/usr/local/bin/<name>`             |
| `SERVICE_NAME`  | `mtg`                                | systemd unit name                                                               |
| `EXEC_ARGS`     | `simple-run 0.0.0.0:{port} {secret}` | Arguments for `ExecStart`. `{port}` and `{secret}` come from the form; the page asks only for the ones used here |
| `PORT`          | `8080`                               | Port the web page listens on                                                    |
| `BASE_PATH`     | *(empty)*                            | Path prefix the panel serves the page under, e.g. `/api/plugin/view/mtproto-remote` |
| `DATA_DIR`      | `/data`                              | Where confirmed host keys are kept; mount it to keep them across restarts       |

The page's **Generate** button makes an mtg FakeTLS secret: `ee` + 16 random bytes +
the hex of the domain. Secrets may contain only letters, digits and `-_.=+/:,@`. This
keeps them safe inside a systemd `ExecStart` line.

## Images

GitHub Actions builds `ghcr.io/nasnet-community/mtproto-remote-plugin`, one tag per
architecture, the same way `nasnet-monitor` does:

- `main-amd64`, `main-arm64`: built from `main`
- `<version>-amd64`, `<version>-arm64`: built from `v*.*.*` tags

The build fails if the binary is 10 MB or more.

## Development

```sh
cargo test                  # unit and end-to-end tests
cargo test -- --ignored     # also downloads the real mtg release from GitHub
cargo run                   # page at http://localhost:8080
```

The end-to-end tests don't need a real server. They start an SSH server inside the test
process. That server runs each command in a local shell whose `PATH` starts with fake
`sudo`, `systemctl`, `install`, `id` and `uname`, and those fakes write under a temp
directory. So the tests cover the SSH protocol, the upload, the sudo password handling
and the unit contents without touching the machine.

To build a static binary locally without zig:

```sh
rustup target add x86_64-unknown-linux-musl
CC_x86_64_unknown_linux_musl=gcc cargo build --release --target x86_64-unknown-linux-musl
```
