# mtproto-remote-plugin

A NASNET Panel plugin that runs Telegram MTProto proxies on **your own servers** and
manages them from the router. It installs [telemt](https://github.com/telemt/telemt) on
any Linux server you can reach over SSH, then manages each server from a web page in the
panel:

- **Users.** Add and delete users. Turn each one on or off. Give a user a new secret,
  which invalidates the old link, and share the link as a `tg://` or `t.me` link or a QR
  code.
- **Per-user limits.** Traffic quota (and resetting it), expiry date, how many devices
  (distinct IPs) and how many connections, and download/upload speed.
- **Live numbers.** For each user: connections, active client IPs and total traffic.
  Totals for each server.
- **The service.** Status, version and uptime. Start, stop and restart. Read the journal,
  update telemt to the configured release, or uninstall it.
- **Disguise domain.** Change the FakeTLS domain the proxy traffic imitates.
- **Several servers**, each with its own users.

The container image is one static binary on `scratch`, about 3.7 MB. The React page is
embedded in that binary (74 KB gzipped).

## How it works

### Adding a server

You give a server address, SSH login, proxy port, a disguise domain and a first user.
Then:

1. **Host key check.** The router reads the server's SSH host key and shows its SHA256
   fingerprint for you to confirm. After that, every connection must present the same
   key.
2. **Login.** The router logs in with the password. If the server has turned off plain
   password login, it tries keyboard-interactive login instead. For a non-root user,
   every root step goes through `sudo` with the same password.
3. **Download.** The router downloads telemt's static `linux-musl` release for the
   server's architecture (x86_64 or aarch64) from GitHub and unpacks it. The server never
   needs to reach GitHub.
4. **Install.** The router installs telemt to run as its own `telemt` system user, then
   enables and starts its systemd unit. The config binds telemt's control API to
   `127.0.0.1` only and protects it with a random bearer token. If the server already has
   a telemt config from an earlier install, it is kept along with its users.
5. **Router key.** The router adds its own ed25519 key to the login user's
   `~/.ssh/authorized_keys`, then proves the key works by logging in with it. From then
   on it only uses the key. **The password is used for this install only and is never
   stored.**
6. **Save.** The router waits for telemt's API to answer, then saves the server.

### Managing a server

- **Users and settings.** Every action logs in with the router key. The router reaches
  telemt's API through that SSH connection (a direct-tcpip channel to
  `127.0.0.1:9091`), so the API is never exposed to the network. Only an allowlist of
  API routes can be called: users, system info, stats and the config.
- **Service actions.** Start, stop, restart, logs, update and uninstall run through the
  root helper `/usr/local/sbin/nasnet-telemt-ctl`. A non-root login user may run that
  one file through sudo without a password, via `/etc/sudoers.d/nasnet-telemt`, and
  nothing else.

### Removing a server

You can **uninstall** (stops telemt and deletes it, its config and every user) or **only
remove the server from the list** (telemt keeps running). Either way, the router's key
is removed from `authorized_keys`.

### Files on each server

| Path                                  | What                                     |
| ------------------------------------- | ---------------------------------------- |
| `/usr/local/bin/telemt`               | telemt binary                            |
| `/etc/telemt/telemt.toml`             | Config; telemt rewrites it as users change |
| `/var/lib/telemt/`                    | telemt's working and state directory     |
| `/etc/systemd/system/telemt.service`  | systemd unit                             |
| `/usr/local/sbin/nasnet-telemt-ctl`   | Root helper for service actions          |
| `/etc/sudoers.d/nasnet-telemt`        | Only for a non-root login user           |

### Data on the router (`$DATA_DIR`)

| File               | What                                                         |
| ------------------ | ------------------------------------------------------------ |
| `id_ed25519`       | The router's SSH key (mode 0600)                             |
| `servers.json`     | Managed servers, including each one's API token (mode 0600) |
| `known_hosts.json` | Host keys you have confirmed                                 |

Mount `DATA_DIR` so these survive a container restart. If the key is lost, add each
server again: the router then installs a new key and keeps the existing config and
users.

## Settings

The panel passes these to the container as environment variables. A blank value uses
the default.

| Variable      | Default         | Meaning                                                          |
| ------------- | --------------- | ---------------------------------------------------------------- |
| `GITHUB_REPO` | `telemt/telemt` | Where telemt releases come from (a fork or mirror also works)    |
| `RELEASE_TAG` | *(latest)*      | Pin a telemt release, e.g. `3.5.9`                               |
| `PORT`        | `8080`          | Port the web page listens on                                     |
| `BASE_PATH`   | *(empty)*       | Path prefix the panel serves the page under, e.g. `/api/plugin/view/mtproto-remote` |
| `DATA_DIR`    | `/data`         | Where the router key, servers and host keys are kept             |

## Development

```sh
cd web && npm ci && npm run build && cd ..   # build the page (the binary embeds web/dist)
cargo test                                   # unit and end-to-end tests
cargo test -- --ignored                      # also downloads and runs the real telemt
cargo run                                    # page and API at http://localhost:8080

cd web && npm run dev:mock                   # UI only, with a fake API and sample data
```

If you run `cargo build` before the page is built, the binary embeds a placeholder
page, so the Rust tests don't need Node.

**How the end-to-end tests work.** They need no real server. They start an SSH server
inside the test process that runs each command in a local shell. That shell's `PATH`
starts with fake `sudo`, `systemctl`, `journalctl`, `useradd`, `visudo` and `install`,
and telemt's files go under a temp directory. A fake telemt API checks the token from
the config the install wrote. The tests cover the whole flow over real SSH: password
and key login, sudo, uploads, the sudoers rule, the tunnel to the API, user management,
service control, update and uninstall.

**The live test** (`--ignored`) runs the real telemt release with the config this app
generates. It drives every user operation through the SSH tunnel.

To build a static binary locally without zig:

```sh
rustup target add x86_64-unknown-linux-musl
CC_x86_64_unknown_linux_musl=gcc cargo build --release --target x86_64-unknown-linux-musl
```

## Images

GitHub Actions builds `ghcr.io/nasnet-community/mtproto-remote-plugin`, with one tag per
architecture, the same way `nasnet-monitor` does:

- `main-amd64`, `main-arm64`: built from `main`
- `<version>-amd64`, `<version>-arm64`: built from `v*.*.*` tags

The build fails if the binary is 10 MB or more.
