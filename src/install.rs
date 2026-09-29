//! Remote commands that put the binary and its systemd unit on the server.

use std::time::Duration;

use thiserror::Error;

use crate::config::Target;
use crate::ssh::{Output, Session, SshError};

pub const BIN_DIR: &str = "/usr/local/bin";
pub const UNIT_DIR: &str = "/etc/systemd/system";

#[derive(Debug, Error)]
pub enum InstallError {
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

fn check(what: &str, output: Output) -> Result<Output, InstallError> {
    if output.success() {
        Ok(output)
    } else {
        Err(InstallError::Command {
            what: what.to_string(),
            output,
        })
    }
}

/// Quotes `s` as one POSIX shell word.
pub fn shell_quote(s: &str) -> String {
    format!("'{}'", s.replace('\'', r"'\''"))
}

/// True if `s` is safe to place unquoted in a systemd ExecStart line.
pub fn is_plain_arg(s: &str) -> bool {
    !s.is_empty()
        && s.len() <= 1024
        && s.chars()
            .all(|c| c.is_ascii_alphanumeric() || "-_.=+/:,@".contains(c))
}

/// Fills `{port}` and `{secret}` in the ExecStart argument template.
pub fn render_exec_args(template: &str, port: u16, secret: &str) -> String {
    template
        .replace("{port}", &port.to_string())
        .replace("{secret}", secret)
}

pub fn binary_path(target: &Target) -> String {
    format!("{BIN_DIR}/{}", target.binary_name)
}

pub fn unit_path(target: &Target) -> String {
    format!("{UNIT_DIR}/{}.service", target.service_name)
}

pub fn unit_file(target: &Target, release_tag: &str, exec_args: &str) -> String {
    let exec = [binary_path(target), exec_args.trim().to_string()]
        .into_iter()
        .filter(|s| !s.is_empty())
        .collect::<Vec<_>>()
        .join(" ");
    format!(
        "\
[Unit]
Description={service} ({repo} {release_tag}, installed by NASNET mtproto-remote)
After=network-online.target
Wants=network-online.target

[Service]
ExecStart={exec}
Restart=always
RestartSec=3
DynamicUser=yes
StateDirectory={service}
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=yes
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
",
        service = target.service_name,
        repo = target.repo,
    )
}

/// What the server reported about itself.
#[derive(Debug, Clone, PartialEq)]
pub struct ServerInfo {
    pub uname_m: String,
    pub is_root: bool,
}

/// A logged-in session plus how to run commands as root on it.
pub struct Remote<'a> {
    session: &'a Session,
    /// `None` when logged in as root; otherwise the sudo password.
    sudo_password: Option<String>,
}

impl<'a> Remote<'a> {
    /// Checks the server can take the install, and that sudo works if needed.
    pub async fn prepare(
        session: &'a Session,
        password: &str,
    ) -> Result<(Remote<'a>, ServerInfo), InstallError> {
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
            return Err(InstallError::Unsupported(format!(
                "unexpected server reply: {:?}",
                out.stdout
            )));
        };
        if init != "systemd" {
            return Err(InstallError::Unsupported(
                "the server has no systemctl; only systemd servers are supported".into(),
            ));
        }
        let info = ServerInfo {
            uname_m: uname_m.to_string(),
            is_root: uid == "0",
        };
        let remote = Remote {
            session,
            sudo_password: (!info.is_root).then(|| password.to_string()),
        };
        if !info.is_root {
            remote
                .run_as_root("Using sudo (the user needs sudo rights)", "true")
                .await?;
        }
        Ok((remote, info))
    }

    /// Runs a shell script as root, through sudo when not logged in as root.
    pub async fn run_as_root(&self, what: &str, script: &str) -> Result<Output, InstallError> {
        let out = match &self.sudo_password {
            None => {
                self.session
                    .exec(&format!("sh -c {}", shell_quote(script)), b"")
                    .await?
            }
            Some(password) => {
                self.session
                    .exec(
                        &format!("sudo -S -p '' sh -c {}", shell_quote(script)),
                        format!("{password}\n").as_bytes(),
                    )
                    .await?
            }
        };
        check(what, out)
    }

    /// Copies `bytes` to a new temp file on the server and returns its path.
    pub async fn upload(&self, what: &str, bytes: &[u8]) -> Result<String, InstallError> {
        let out = check(
            what,
            self.session
                .exec("mktemp /tmp/mtproto-remote.XXXXXX", b"")
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
            return Err(InstallError::Unsupported(format!(
                "{what}: server received {} bytes, expected {}",
                out.stdout.trim(),
                bytes.len()
            )));
        }
        Ok(path)
    }

    pub async fn remove_temp(&self, paths: &[String]) {
        let quoted: Vec<String> = paths.iter().map(|p| shell_quote(p)).collect();
        let _ = self
            .session
            .exec(&format!("rm -f {}", quoted.join(" ")), b"")
            .await;
    }

    /// Stops the old service and puts the binary in place.
    pub async fn install_binary(&self, target: &Target, tmp: &str) -> Result<(), InstallError> {
        let script = format!(
            "systemctl stop {svc} 2>/dev/null; install -m 0755 {tmp} {bin}",
            svc = shell_quote(&target.service_name),
            tmp = shell_quote(tmp),
            bin = shell_quote(&binary_path(target)),
        );
        self.run_as_root("Installing the binary", &script).await?;
        Ok(())
    }

    /// Installs the unit, then enables and (re)starts the service.
    pub async fn install_service(&self, target: &Target, tmp: &str) -> Result<(), InstallError> {
        let svc = shell_quote(&target.service_name);
        let script = format!(
            "install -m 0644 {tmp} {unit} && systemctl daemon-reload \
             && systemctl enable {svc} && systemctl restart {svc}",
            tmp = shell_quote(tmp),
            unit = shell_quote(&unit_path(target)),
        );
        self.run_as_root("Starting the service", &script).await?;
        Ok(())
    }

    /// Waits for the service to settle and returns `(active, status text)`.
    pub async fn service_status(
        &self,
        target: &Target,
        settle: Duration,
    ) -> Result<(bool, String), InstallError> {
        tokio::time::sleep(settle).await;
        let svc = shell_quote(&target.service_name);
        let active = self
            .session
            .exec(&format!("systemctl is-active {svc}"), b"")
            .await?;
        let status = self
            .run_as_root(
                "Reading the service status",
                &format!("systemctl status {svc} --no-pager -l -n 20; true"),
            )
            .await?;
        Ok((active.stdout.trim() == "active", status.stdout))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn quotes_shell_words() {
        assert_eq!(shell_quote("plain"), "'plain'");
        assert_eq!(shell_quote("it's"), r"'it'\''s'");
        assert_eq!(shell_quote(""), "''");
    }

    #[test]
    fn plain_args_exclude_shell_and_systemd_specials() {
        assert!(is_plain_arg("ee0123abcdef676f6f676c652e636f6d"));
        assert!(is_plain_arg("7rZ+Qx/0Yw=="));
        for bad in [
            "", "a b", "a;b", "$HOME", "%n", "a\"b", "a'b", "a\nb", "a\\b",
        ] {
            assert!(!is_plain_arg(bad), "accepted {bad:?}");
        }
    }

    #[test]
    fn unit_runs_the_binary_with_rendered_args() {
        let target = Target::default();
        let args = render_exec_args(&target.exec_args, 443, "ee00");
        assert_eq!(args, "simple-run 0.0.0.0:443 ee00");
        let unit = unit_file(&target, "v2.2.8", &args);
        assert!(unit.contains("\nExecStart=/usr/local/bin/mtg simple-run 0.0.0.0:443 ee00\n"));
        assert!(unit.contains("Description=mtg (9seconds/mtg v2.2.8,"));
        assert!(unit.contains("\nWantedBy=multi-user.target\n"));
    }

    #[test]
    fn unit_without_args_has_no_trailing_space() {
        let unit = unit_file(&Target::default(), "v1", "");
        assert!(unit.contains("\nExecStart=/usr/local/bin/mtg\n"));
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
}
