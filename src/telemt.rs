//! How telemt is laid out on a server, and the remote commands that install,
//! run and remove it.

use thiserror::Error;

use crate::ssh::{Output, Session, SshError};

/// systemd unit and system user name.
pub const SERVICE: &str = "telemt";

#[derive(Debug, Error)]
pub enum RemoteError {
    #[error(transparent)]
    Ssh(#[from] SshError),
    #[error("{what} failed{}", describe(.output))]
    Command { what: String, output: Output },
    #[error("{0}")]
    Unsupported(String),
}

fn describe(output: &Output) -> String {
    let code = output
        .code
        .map_or("no exit status".to_string(), |c| format!("exit {c}"));
    let text = [output.stderr.trim(), output.stdout.trim()]
        .into_iter()
        .filter(|s| !s.is_empty())
        .collect::<Vec<_>>()
        .join("\n");
    if text.is_empty() {
        format!(" ({code})")
    } else {
        format!(" ({code}): {text}")
    }
}

fn check(what: &str, output: Output) -> Result<Output, RemoteError> {
    if output.success() {
        Ok(output)
    } else {
        Err(RemoteError::Command {
            what: what.to_string(),
            output,
        })
    }
}

/// Quotes `s` as one POSIX shell word.
pub fn shell_quote(s: &str) -> String {
    format!("'{}'", s.replace('\'', r"'\''"))
}

/// Letters, digits and `-_.`; safe unquoted in shell, TOML and sudoers.
pub fn is_simple_name(s: &str, max: usize) -> bool {
    !s.is_empty()
        && s.len() <= max
        && !s.starts_with('-')
        && s.chars()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, '-' | '_' | '.'))
}

/// A hostname, IPv4 or IPv6 address.
pub fn is_host(s: &str) -> bool {
    !s.is_empty()
        && s.len() <= 253
        && !s.starts_with('-')
        && s.chars()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, '.' | '-' | ':'))
}

/// Where telemt lives on the server. `root` prefixes every path; it is empty
/// in production and a scratch directory in tests.
#[derive(Debug, Clone)]
pub struct Layout {
    pub root: String,
    /// Loopback port of telemt's control API.
    pub api_port: u16,
}

impl Default for Layout {
    fn default() -> Self {
        Self {
            root: String::new(),
            api_port: 9091,
        }
    }
}

impl Layout {
    fn path(&self, p: &str) -> String {
        format!("{}{p}", self.root)
    }
    pub fn bin(&self) -> String {
        self.path("/usr/local/bin/telemt")
    }
    pub fn ctl(&self) -> String {
        self.path("/usr/local/sbin/nasnet-telemt-ctl")
    }
    pub fn conf_dir(&self) -> String {
        self.path("/etc/telemt")
    }
    pub fn conf(&self) -> String {
        self.path("/etc/telemt/telemt.toml")
    }
    pub fn state_dir(&self) -> String {
        self.path("/var/lib/telemt")
    }
    pub fn unit(&self) -> String {
        self.path("/etc/systemd/system/telemt.service")
    }
    pub fn sudoers(&self) -> String {
        self.path("/etc/sudoers.d/nasnet-telemt")
    }
}

/// Settings for a fresh telemt config.
#[derive(Debug, Clone)]
pub struct NewConfig {
    /// Host put in client links.
    pub public_host: String,
    pub port: u16,
    pub tls_domain: String,
    pub first_user: String,
    /// 32 hex characters.
    pub first_secret: String,
    pub api_token: String,
}

pub fn config_toml(layout: &Layout, c: &NewConfig) -> String {
    format!(
        r#"# Written by NASNET mtproto-remote. telemt rewrites this file when users
# are changed through its API, so edits here may be reformatted.

[general]
log_level = "normal"

[general.modes]
classic = false
secure = false
tls = true

[general.links]
show = "*"
public_host = "{host}"
public_port = {port}

[server]
port = {port}

[server.api]
enabled = true
listen = "127.0.0.1:{api_port}"
whitelist = ["127.0.0.1/32", "::1/128"]
auth_header = "Bearer {token}"

[[server.listeners]]
ip = "0.0.0.0"

[censorship]
tls_domain = "{domain}"
mask = true
tls_emulation = true
tls_front_dir = "tlsfront"

[access.users]
"{user}" = "{secret}"
"#,
        host = c.public_host,
        port = c.port,
        api_port = layout.api_port,
        token = c.api_token,
        domain = c.tls_domain,
        user = c.first_user,
        secret = c.first_secret,
    )
}

pub fn unit_file(layout: &Layout, release_tag: &str) -> String {
    format!(
        "\
[Unit]
Description=telemt MTProto proxy {release_tag} (installed by NASNET mtproto-remote)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User={SERVICE}
Group={SERVICE}
WorkingDirectory={state}
ExecStart={bin} run {conf}
Restart=on-failure
RestartSec=3
LimitNOFILE=65536
AmbientCapabilities=CAP_NET_ADMIN CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_BIND_SERVICE
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
",
        state = layout.state_dir(),
        bin = layout.bin(),
        conf = layout.conf(),
    )
}

/// Root helper the router runs for service control after install. A non-root
/// login user may run exactly this file through sudo without a password.
pub fn ctl_script(layout: &Layout) -> String {
    format!(
        r#"#!/bin/sh
# Installed by NASNET mtproto-remote: the root actions the router may run.
set -e
case "$1" in
  start|stop|restart)
    exec systemctl "$1" {SERVICE} ;;
  logs)
    lines=$2
    case "$lines" in ''|*[!0-9]*) lines=200 ;; esac
    exec journalctl -u {SERVICE} --no-pager -o short-iso -n "$lines" ;;
  update)
    [ -f "$2" ] || {{ echo "no such file: $2" >&2; exit 2; }}
    install -m 0755 "$2" {bin}
    rm -f "$2"
    exec systemctl restart {SERVICE} ;;
  uninstall)
    systemctl disable --now {SERVICE} >/dev/null 2>&1 || true
    rm -f {unit} {bin}
    systemctl daemon-reload
    rm -rf {conf_dir} {state}
    userdel {SERVICE} >/dev/null 2>&1 || true
    rm -f {sudoers} "$0"
    echo uninstalled ;;
  *)
    echo "usage: $0 start|stop|restart|logs [lines]|update <file>|uninstall" >&2
    exit 2 ;;
esac
"#,
        bin = shell_quote(&layout.bin()),
        unit = shell_quote(&layout.unit()),
        conf_dir = shell_quote(&layout.conf_dir()),
        state = shell_quote(&layout.state_dir()),
        sudoers = shell_quote(&layout.sudoers()),
    )
}

pub fn sudoers_rule(layout: &Layout, user: &str) -> String {
    format!(
        "# Installed by NASNET mtproto-remote.\n{user} ALL=(root) NOPASSWD: {}\n",
        layout.ctl()
    )
}

/// Temp files already uploaded for [`Remote::install`].
pub struct Uploads {
    pub binary: String,
    pub config: String,
    pub ctl: String,
    pub unit: String,
    /// Only for a non-root login.
    pub sudoers: Option<String>,
}

/// What [`Remote::install`] found.
#[derive(Debug, Clone, PartialEq)]
pub struct Installed {
    /// A config was already there and was left untouched.
    pub kept_config: bool,
    /// The API token in the config that is now in place.
    pub api_token: String,
}

pub fn install_script(layout: &Layout, up: &Uploads) -> String {
    let q = |s: &str| shell_quote(s);
    let mut script = format!(
        "set -e
id -u {SERVICE} >/dev/null 2>&1 || useradd --system --user-group --no-create-home --home-dir {state} --shell /bin/false {SERVICE}
install -d -m 0750 -o {SERVICE} -g {SERVICE} {conf_dir} {state}
if [ -f {conf} ]; then echo kept; else install -m 0640 -o {SERVICE} -g {SERVICE} {config} {conf}; echo created; fi
systemctl stop {SERVICE} >/dev/null 2>&1 || true
mkdir -p \"$(dirname {bin})\" \"$(dirname {ctl_path})\" \"$(dirname {unit_path})\"
install -m 0755 {binary} {bin}
install -m 0755 {ctl} {ctl_path}
install -m 0644 {unit} {unit_path}
",
        state = q(&layout.state_dir()),
        conf_dir = q(&layout.conf_dir()),
        conf = q(&layout.conf()),
        config = q(&up.config),
        binary = q(&up.binary),
        bin = q(&layout.bin()),
        ctl = q(&up.ctl),
        ctl_path = q(&layout.ctl()),
        unit = q(&up.unit),
        unit_path = q(&layout.unit()),
    );
    if let Some(sudoers) = &up.sudoers {
        script.push_str(&format!(
            "mkdir -p \"$(dirname {dst})\"
install -m 0440 {src} {dst}
visudo -cf {dst} >/dev/null || {{ rm -f {dst}; echo 'visudo rejected the sudoers rule' >&2; exit 1; }}
",
            src = q(sudoers),
            dst = q(&layout.sudoers()),
        ));
    }
    script.push_str(&format!(
        "systemctl daemon-reload
systemctl enable {SERVICE} >/dev/null 2>&1
systemctl restart {SERVICE}
sed -n 's/^auth_header = \"Bearer \\([0-9A-Za-z]*\\)\"$/\\1/p' {conf}
",
        conf = q(&layout.conf()),
    ));
    script
}

fn authorized_keys_script(line: &str, add: bool) -> String {
    let line = shell_quote(line);
    if add {
        format!(
            r#"umask 077; f="$HOME/.ssh/authorized_keys"; mkdir -p "$HOME/.ssh" && touch "$f" && if ! grep -qxF {line} "$f"; then if [ -s "$f" ] && [ -n "$(tail -c1 "$f")" ]; then echo >> "$f"; fi; printf '%s\n' {line} >> "$f"; fi"#
        )
    } else {
        format!(
            r#"f="$HOME/.ssh/authorized_keys"; [ -f "$f" ] || exit 0; {{ grep -vxF {line} "$f" || true; }} > "$f.nasnet" && cat "$f.nasnet" > "$f" && rm -f "$f.nasnet""#
        )
    }
}

/// What the server reported about itself.
#[derive(Debug, Clone, PartialEq)]
pub struct ServerInfo {
    pub uname_m: String,
    pub is_root: bool,
}

enum Privilege {
    Root,
    /// sudo, answering its prompt with the login password.
    SudoPassword(String),
    /// sudo -n, allowed by the sudoers rule for the ctl helper.
    SudoRule,
}

/// A logged-in session plus how to run commands as root on it.
pub struct Remote<'a> {
    session: &'a Session,
    layout: &'a Layout,
    privilege: Privilege,
}

impl<'a> Remote<'a> {
    /// For install: checks the server and that sudo works with `password`.
    pub async fn prepare(
        session: &'a Session,
        layout: &'a Layout,
        password: &str,
    ) -> Result<(Remote<'a>, ServerInfo), RemoteError> {
        let out = check(
            "Reading server details",
            session
                .exec(
                    "uname -m; id -u; command -v systemctl >/dev/null && echo systemd || echo none",
                    b"",
                )
                .await?,
        )?;
        let lines: Vec<&str> = out.stdout.lines().map(str::trim).collect();
        let [uname_m, uid, init] = lines[..] else {
            return Err(RemoteError::Unsupported(format!(
                "unexpected server reply: {:?}",
                out.stdout
            )));
        };
        if init != "systemd" {
            return Err(RemoteError::Unsupported(
                "the server has no systemctl; only systemd servers are supported".into(),
            ));
        }
        let info = ServerInfo {
            uname_m: uname_m.to_string(),
            is_root: uid == "0",
        };
        let privilege = if info.is_root {
            Privilege::Root
        } else {
            Privilege::SudoPassword(password.to_string())
        };
        let remote = Remote {
            session,
            layout,
            privilege,
        };
        if !info.is_root {
            remote
                .run_as_root("Using sudo (the user needs sudo rights)", "true")
                .await?;
        }
        Ok((remote, info))
    }

    /// For management over the router key, after install.
    pub fn managed(session: &'a Session, layout: &'a Layout, is_root: bool) -> Remote<'a> {
        let privilege = if is_root {
            Privilege::Root
        } else {
            Privilege::SudoRule
        };
        Remote {
            session,
            layout,
            privilege,
        }
    }

    /// Runs a shell script as root with the install password.
    async fn run_as_root(&self, what: &str, script: &str) -> Result<Output, RemoteError> {
        let command = format!("sh -c {}", shell_quote(script));
        let out = match &self.privilege {
            Privilege::Root => self.session.exec(&command, b"").await?,
            Privilege::SudoPassword(password) => {
                self.session
                    .exec(
                        &format!("sudo -S -p '' {command}"),
                        format!("{password}\n").as_bytes(),
                    )
                    .await?
            }
            Privilege::SudoRule => {
                return Err(RemoteError::Unsupported(format!(
                    "{what} needs the server password"
                )));
            }
        };
        check(what, out)
    }

    /// Runs the ctl helper as root: `start`, `stop`, `restart`, `logs N`, …
    pub async fn ctl(&self, what: &str, args: &[&str]) -> Result<Output, RemoteError> {
        let mut command = shell_quote(&self.layout.ctl());
        for arg in args {
            command.push(' ');
            command.push_str(&shell_quote(arg));
        }
        let command = match self.privilege {
            Privilege::Root => command,
            _ => format!("sudo -n {command}"),
        };
        check(what, self.session.exec(&command, b"").await?)
    }

    /// Copies `bytes` to a new temp file on the server and returns its path.
    pub async fn upload(&self, what: &str, bytes: &[u8]) -> Result<String, RemoteError> {
        let out = check(
            what,
            self.session
                .exec("mktemp /tmp/nasnet-telemt.XXXXXX", b"")
                .await?,
        )?;
        let path = out.stdout.trim().to_string();
        let out = check(
            what,
            self.session
                .exec(
                    &format!("cat > {p} && wc -c < {p}", p = shell_quote(&path)),
                    bytes,
                )
                .await?,
        )?;
        if out.stdout.trim() != bytes.len().to_string() {
            return Err(RemoteError::Unsupported(format!(
                "{what}: server received {} bytes, expected {}",
                out.stdout.trim(),
                bytes.len()
            )));
        }
        Ok(path)
    }

    pub async fn remove_temp(&self, paths: &[String]) {
        if paths.is_empty() {
            return;
        }
        let quoted: Vec<String> = paths.iter().map(|p| shell_quote(p)).collect();
        let _ = self
            .session
            .exec(&format!("rm -f {}", quoted.join(" ")), b"")
            .await;
    }

    /// Installs (or reinstalls) telemt and starts it.
    pub async fn install(&self, uploads: &Uploads) -> Result<Installed, RemoteError> {
        let out = self
            .run_as_root("Installing telemt", &install_script(self.layout, uploads))
            .await?;
        let lines: Vec<&str> = out.stdout.lines().map(str::trim).collect();
        let kept_config = lines.first() == Some(&"kept");
        let api_token = lines
            .last()
            .filter(|t| lines.len() > 1 && !t.is_empty())
            .map(|t| t.to_string())
            .ok_or_else(|| {
                RemoteError::Unsupported(format!(
                    "{} has no usable [server.api] auth_header; remove it to let a fresh config be written",
                    self.layout.conf()
                ))
            })?;
        Ok(Installed {
            kept_config,
            api_token,
        })
    }

    /// Adds `line` to the login user's authorized_keys (once).
    pub async fn authorize_key(&self, line: &str) -> Result<(), RemoteError> {
        let out = self
            .session
            .exec(&authorized_keys_script(line, true), b"")
            .await?;
        check("Adding the router key to authorized_keys", out)?;
        Ok(())
    }

    /// Removes `line` from the login user's authorized_keys.
    pub async fn revoke_key(&self, line: &str) -> Result<(), RemoteError> {
        let out = self
            .session
            .exec(&authorized_keys_script(line, false), b"")
            .await?;
        check("Removing the router key from authorized_keys", out)?;
        Ok(())
    }

    /// `systemctl is-active telemt`, which needs no privileges.
    pub async fn is_active(&self) -> Result<String, RemoteError> {
        let out = self
            .session
            .exec(&format!("systemctl is-active {SERVICE}"), b"")
            .await?;
        Ok(out.stdout.trim().to_string())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn new_config() -> NewConfig {
        NewConfig {
            public_host: "203.0.113.10".into(),
            port: 443,
            tls_domain: "www.google.com".into(),
            first_user: "me".into(),
            first_secret: "0123456789abcdef0123456789abcdef".into(),
            api_token: "tok123".into(),
        }
    }

    #[test]
    fn quotes_shell_words() {
        assert_eq!(shell_quote("plain"), "'plain'");
        assert_eq!(shell_quote("it's"), r"'it'\''s'");
    }

    #[test]
    fn validates_names_and_hosts() {
        assert!(is_simple_name("alice.b-2_c", 64));
        for bad in ["", "-x", "a b", "a;b", "a\"b", "é"] {
            assert!(!is_simple_name(bad, 64), "accepted {bad:?}");
        }
        assert!(is_host("vps.example.com") && is_host("2001:db8::1") && is_host("10.0.0.1"));
        assert!(!is_host("a b") && !is_host("-oProxy") && !is_host(""));
    }

    #[test]
    fn config_binds_the_api_to_loopback_with_a_token() {
        let toml = config_toml(&Layout::default(), &new_config());
        assert!(toml.contains("\nlisten = \"127.0.0.1:9091\"\n"));
        assert!(toml.contains("\nauth_header = \"Bearer tok123\"\n"));
        assert!(toml.contains("\npublic_host = \"203.0.113.10\"\npublic_port = 443\n"));
        assert!(toml.contains("\n[server]\nport = 443\n"));
        assert!(toml.contains("\n\"me\" = \"0123456789abcdef0123456789abcdef\"\n"));
        assert!(toml.contains("\ntls_domain = \"www.google.com\"\n"));
    }

    #[test]
    fn unit_runs_telemt_as_its_own_user() {
        let unit = unit_file(&Layout::default(), "3.5.9");
        assert!(unit.contains("\nExecStart=/usr/local/bin/telemt run /etc/telemt/telemt.toml\n"));
        assert!(unit.contains("\nUser=telemt\n"));
        assert!(unit.contains("\nWorkingDirectory=/var/lib/telemt\n"));
    }

    #[test]
    fn sudoers_allows_only_the_helper() {
        assert_eq!(
            sudoers_rule(&Layout::default(), "deploy"),
            "# Installed by NASNET mtproto-remote.\ndeploy ALL=(root) NOPASSWD: /usr/local/sbin/nasnet-telemt-ctl\n"
        );
    }

    #[test]
    fn paths_follow_the_layout_root() {
        let layout = Layout {
            root: "/tmp/x".into(),
            api_port: 1,
        };
        assert_eq!(layout.conf(), "/tmp/x/etc/telemt/telemt.toml");
        assert!(ctl_script(&layout).contains("'/tmp/x/usr/local/bin/telemt'"));
    }

    #[test]
    fn describes_failed_commands() {
        let output = Output {
            code: Some(1),
            stdout: String::new(),
            stderr: "sudo: a password is required\n".into(),
        };
        let err = check("Using sudo", output).unwrap_err();
        assert_eq!(
            err.to_string(),
            "Using sudo failed (exit 1): sudo: a password is required"
        );
    }

    /// Runs the authorized_keys scripts with a local shell.
    fn run_local(script: &str, home: &std::path::Path) -> std::process::Output {
        std::process::Command::new("sh")
            .arg("-c")
            .arg(script)
            .env("HOME", home)
            .output()
            .unwrap()
    }

    #[test]
    fn authorized_keys_edits_are_idempotent_and_keep_other_keys() {
        let home = std::env::temp_dir().join(format!("ak-test-{}", std::process::id()));
        std::fs::create_dir_all(home.join(".ssh")).unwrap();
        let file = home.join(".ssh/authorized_keys");
        // An existing key without a trailing newline must not be joined to ours.
        std::fs::write(&file, "ssh-ed25519 AAAAother user@laptop").unwrap();

        let ours = "ssh-ed25519 AAAAours nasnet-mtproto-remote";
        for _ in 0..2 {
            assert!(
                run_local(&authorized_keys_script(ours, true), &home)
                    .status
                    .success()
            );
        }
        assert_eq!(
            std::fs::read_to_string(&file).unwrap(),
            format!("ssh-ed25519 AAAAother user@laptop\n{ours}\n")
        );

        assert!(
            run_local(&authorized_keys_script(ours, false), &home)
                .status
                .success()
        );
        assert_eq!(
            std::fs::read_to_string(&file).unwrap(),
            "ssh-ed25519 AAAAother user@laptop\n"
        );
        std::fs::remove_dir_all(home).unwrap();
    }
}
