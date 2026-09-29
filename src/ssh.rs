//! A small SSH client: pinned host key, password or key login, exec with
//! stdin, and HTTP to a loopback port on the server through the connection.

use std::sync::{Arc, Mutex};
use std::time::Duration;

use russh::client::{self, Handle, KeyboardInteractiveAuthResponse};
use russh::keys::{HashAlg, PrivateKey, PrivateKeyWithHashAlg, PublicKeyOrCertificate};
use russh::{ChannelMsg, Disconnect};
use thiserror::Error;

use crate::http;

const CONNECT_TIMEOUT: Duration = Duration::from_secs(15);

#[derive(Debug, Error)]
pub enum SshError {
    #[error("could not connect to {addr}: {reason}")]
    Connect { addr: String, reason: String },
    #[error("timed out connecting to {0}")]
    Timeout(String),
    #[error(
        "the server's host key is {actual}, not the confirmed {expected}; refusing to continue"
    )]
    HostKeyMismatch { expected: String, actual: String },
    #[error("login failed for {0}: wrong username or password, or password login is disabled")]
    AuthFailed(String),
    #[error(
        "the server no longer accepts the router key for {0}; it was removed from authorized_keys, or key login is disabled"
    )]
    KeyRejected(String),
    #[error("request to the server's port {port} failed: {reason}")]
    Tunnel { port: u16, reason: String },
    #[error("ssh: {0}")]
    Protocol(#[from] russh::Error),
}

/// What a remote command produced.
#[derive(Debug, Default, Clone, PartialEq)]
pub struct Output {
    /// `None` if the server closed the channel without an exit status.
    pub code: Option<u32>,
    pub stdout: String,
    pub stderr: String,
}

impl Output {
    pub fn success(&self) -> bool {
        self.code == Some(0)
    }
}

/// SHA256 fingerprint in OpenSSH's `SHA256:<base64>` form.
pub fn fingerprint(key: &PublicKeyOrCertificate) -> String {
    match key {
        PublicKeyOrCertificate::PublicKey { key, .. } => key.fingerprint(HashAlg::Sha256),
        PublicKeyOrCertificate::Certificate(cert) => cert.public_key().fingerprint(HashAlg::Sha256),
    }
    .to_string()
}

struct Client {
    seen: Arc<Mutex<Option<String>>>,
    expected: Option<String>,
}

impl client::Handler for Client {
    type Error = russh::Error;

    async fn check_server_key(
        &mut self,
        key: &PublicKeyOrCertificate,
    ) -> Result<bool, Self::Error> {
        let actual = fingerprint(key);
        let accepted = self.expected.as_deref() == Some(actual.as_str());
        *self.seen.lock().expect("fingerprint lock") = Some(actual);
        Ok(accepted)
    }
}

fn client_config() -> Arc<client::Config> {
    Arc::new(client::Config {
        inactivity_timeout: Some(Duration::from_secs(120)),
        keepalive_interval: Some(Duration::from_secs(15)),
        ..Default::default()
    })
}

async fn open(
    host: &str,
    port: u16,
    expected: Option<String>,
) -> (Result<Handle<Client>, SshError>, Option<String>) {
    let addr = format!("{host}:{port}");
    let seen = Arc::new(Mutex::new(None));
    let handler = Client {
        seen: seen.clone(),
        expected,
    };
    let result = tokio::time::timeout(
        CONNECT_TIMEOUT,
        client::connect(client_config(), (host, port), handler),
    )
    .await;
    let seen = seen.lock().expect("fingerprint lock").clone();
    let result = match result {
        Err(_) => Err(SshError::Timeout(addr)),
        Ok(Err(e)) => Err(SshError::Connect {
            addr,
            reason: e.to_string(),
        }),
        Ok(Ok(handle)) => Ok(handle),
    };
    (result, seen)
}

async fn open_pinned(host: &str, port: u16, fingerprint: &str) -> Result<Handle<Client>, SshError> {
    match open(host, port, Some(fingerprint.to_string())).await {
        (Ok(handle), _) => Ok(handle),
        (Err(_), Some(actual)) if actual != fingerprint => Err(SshError::HostKeyMismatch {
            expected: fingerprint.to_string(),
            actual,
        }),
        (Err(e), _) => Err(e),
    }
}

/// Reads the server's host key fingerprint without logging in.
pub async fn probe(host: &str, port: u16) -> Result<String, SshError> {
    // No key is expected, so the handshake stops right after the server
    // presents its key; that rejection is the normal outcome here.
    match open(host, port, None).await {
        (_, Some(fingerprint)) => Ok(fingerprint),
        (Err(e), None) => Err(e),
        (Ok(_), None) => unreachable!("handshake accepted without a host key"),
    }
}

pub struct Session {
    handle: Handle<Client>,
}

impl Session {
    /// Connects, requires the host key to equal `fingerprint`, and logs in with
    /// a password (falling back to keyboard-interactive).
    pub async fn connect(
        host: &str,
        port: u16,
        fingerprint: &str,
        user: &str,
        password: &str,
    ) -> Result<Session, SshError> {
        let mut handle = open_pinned(host, port, fingerprint).await?;
        if !handle
            .authenticate_password(user, password)
            .await?
            .success()
            && !keyboard_interactive(&mut handle, user, password).await?
        {
            return Err(SshError::AuthFailed(user.to_string()));
        }
        Ok(Session { handle })
    }

    /// Connects with a pinned host key and logs in with `key`.
    pub async fn connect_with_key(
        host: &str,
        port: u16,
        fingerprint: &str,
        user: &str,
        key: &PrivateKey,
    ) -> Result<Session, SshError> {
        let mut handle = open_pinned(host, port, fingerprint).await?;
        let key = PrivateKeyWithHashAlg::new(Arc::new(key.clone()), None);
        if !handle.authenticate_publickey(user, key).await?.success() {
            return Err(SshError::KeyRejected(user.to_string()));
        }
        Ok(Session { handle })
    }

    /// Sends one HTTP request to `127.0.0.1:port` as seen from the server.
    pub async fn http(
        &self,
        port: u16,
        method: &str,
        path: &str,
        authorization: Option<&str>,
        json_body: Option<&[u8]>,
    ) -> Result<http::Response, SshError> {
        let tunnel = |reason: String| SshError::Tunnel { port, reason };
        let channel = self
            .handle
            .channel_open_direct_tcpip("127.0.0.1", port.into(), "127.0.0.1", 0)
            .await
            .map_err(|e| tunnel(e.to_string()))?;
        let request = http::request(
            channel.into_stream(),
            method,
            path,
            authorization,
            json_body,
        );
        match tokio::time::timeout(Duration::from_secs(30), request).await {
            Ok(Ok(response)) => Ok(response),
            Ok(Err(e)) => Err(tunnel(e.to_string())),
            Err(_) => Err(tunnel("timed out".into())),
        }
    }

    /// False once the connection has dropped.
    pub fn is_open(&self) -> bool {
        !self.handle.is_closed()
    }

    /// Runs `command`, feeding it `stdin` (then EOF), and collects its output.
    pub async fn exec(&self, command: &str, stdin: &[u8]) -> Result<Output, SshError> {
        let mut channel = self.handle.channel_open_session().await?;
        channel.exec(true, command).await?;
        if !stdin.is_empty() {
            channel.data(stdin).await?;
        }
        channel.eof().await?;

        let (mut out, mut stdout, mut stderr) = (Output::default(), Vec::new(), Vec::new());
        while let Some(msg) = channel.wait().await {
            match msg {
                ChannelMsg::Data { data } => stdout.extend_from_slice(&data),
                ChannelMsg::ExtendedData { data, ext: 1 } => stderr.extend_from_slice(&data),
                ChannelMsg::ExitStatus { exit_status } => out.code = Some(exit_status),
                _ => {}
            }
        }
        out.stdout = String::from_utf8_lossy(&stdout).into_owned();
        out.stderr = String::from_utf8_lossy(&stderr).into_owned();
        Ok(out)
    }

    pub async fn close(&self) {
        let _ = self
            .handle
            .disconnect(Disconnect::ByApplication, "", "en")
            .await;
    }
}

/// Answers every keyboard-interactive prompt with the password. Servers with
/// `PasswordAuthentication no` often still take a password this way.
async fn keyboard_interactive(
    handle: &mut Handle<Client>,
    user: &str,
    password: &str,
) -> Result<bool, SshError> {
    let mut reply = handle
        .authenticate_keyboard_interactive_start(user, None)
        .await?;
    for _ in 0..5 {
        match reply {
            KeyboardInteractiveAuthResponse::Success => return Ok(true),
            KeyboardInteractiveAuthResponse::Failure { .. } => return Ok(false),
            KeyboardInteractiveAuthResponse::InfoRequest { prompts, .. } => {
                let answers = vec![password.to_string(); prompts.len()];
                reply = handle
                    .authenticate_keyboard_interactive_respond(answers)
                    .await?;
            }
        }
    }
    Ok(false)
}
