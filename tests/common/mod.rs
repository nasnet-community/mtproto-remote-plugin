//! An in-process SSH server for tests. It runs each exec request through a
//! local `sh -c`, so client code talks to a real SSH server and real commands.
#![allow(dead_code)]

use std::collections::HashMap;
use std::path::PathBuf;
use std::process::Stdio;
use std::sync::Arc;

use russh::keys::{decode_secret_key, ssh_key};
use russh::server::{self, Auth, Msg, Response, Server as _, Session};
use russh::{Channel, ChannelId};
use tokio::io::AsyncWriteExt;
use tokio::net::{TcpListener, TcpStream};
use tokio::process::{ChildStdin, Command};
use tokio::sync::Mutex;

pub const USER: &str = "tester";
pub const PASSWORD: &str = "pw";
/// `ssh-keygen -lf` of tests/fixtures/host_ed25519.
pub const HOST_FINGERPRINT: &str = "SHA256:p2OiwWbYoEAsmmYWEMy1L12ya7lUDzuHYODmtJ18R/w";

#[derive(Clone, Copy, PartialEq)]
pub enum Login {
    Password,
    KeyboardInteractive,
}

#[derive(Clone)]
struct TestServer {
    login: Login,
    /// Public keys accepted for USER are read from this file on each login.
    authorized_keys: Option<Arc<PathBuf>>,
    /// Prepended to every command, e.g. to put fake tools on PATH.
    prelude: Arc<String>,
    stdins: Arc<Mutex<HashMap<ChannelId, ChildStdin>>>,
}

impl server::Server for TestServer {
    type Handler = Self;
    fn new_client(&mut self, _: Option<std::net::SocketAddr>) -> Self {
        self.clone()
    }
}

impl server::Handler for TestServer {
    type Error = russh::Error;

    async fn auth_password(&mut self, user: &str, password: &str) -> Result<Auth, Self::Error> {
        let ok = self.login == Login::Password && user == USER && password == PASSWORD;
        Ok(if ok { Auth::Accept } else { Auth::reject() })
    }

    async fn auth_keyboard_interactive<'a>(
        &'a mut self,
        user: &str,
        _submethods: &str,
        response: Option<Response<'a>>,
    ) -> Result<Auth, Self::Error> {
        if self.login != Login::KeyboardInteractive || user != USER {
            return Ok(Auth::reject());
        }
        match response {
            None => Ok(Auth::Partial {
                name: "".into(),
                instructions: "".into(),
                prompts: vec![("Password: ".into(), false)].into(),
            }),
            Some(answers) => {
                let answers: Vec<_> = answers.collect();
                let ok = answers.len() == 1 && answers[0].as_ref() == PASSWORD.as_bytes();
                Ok(if ok { Auth::Accept } else { Auth::reject() })
            }
        }
    }

    async fn auth_publickey(
        &mut self,
        user: &str,
        key: &ssh_key::PublicKey,
    ) -> Result<Auth, Self::Error> {
        let Some(path) = &self.authorized_keys else {
            return Ok(Auth::reject());
        };
        // Compare "type base64", ignoring the comment, as sshd does.
        let offered = key.to_openssh().unwrap();
        let offered: Vec<&str> = offered.split(' ').take(2).collect();
        let listed = std::fs::read_to_string(path.as_path()).unwrap_or_default();
        let ok = user == USER
            && listed
                .lines()
                .any(|l| l.split(' ').take(2).collect::<Vec<_>>() == offered);
        Ok(if ok { Auth::Accept } else { Auth::reject() })
    }

    /// Forwards the channel to a local TCP port, like sshd's direct-tcpip.
    async fn channel_open_direct_tcpip(
        &mut self,
        channel: Channel<Msg>,
        host: &str,
        port: u32,
        _originator_address: &str,
        _originator_port: u32,
        reply: server::ChannelOpenHandle,
        _session: &mut Session,
    ) -> Result<(), Self::Error> {
        let Ok(mut tcp) = TcpStream::connect((host.to_string(), port as u16)).await else {
            drop(reply);
            return Ok(());
        };
        reply.accept().await;
        tokio::spawn(async move {
            let mut stream = channel.into_stream();
            let _ = tokio::io::copy_bidirectional(&mut stream, &mut tcp).await;
        });
        Ok(())
    }

    async fn channel_open_session(
        &mut self,
        _channel: Channel<Msg>,
        reply: server::ChannelOpenHandle,
        _session: &mut Session,
    ) -> Result<(), Self::Error> {
        reply.accept().await;
        Ok(())
    }

    async fn exec_request(
        &mut self,
        channel: ChannelId,
        data: &[u8],
        session: &mut Session,
    ) -> Result<(), Self::Error> {
        let command = format!("{}{}", self.prelude, String::from_utf8_lossy(data));
        let mut child = Command::new("sh")
            .arg("-c")
            .arg(command)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()
            .expect("spawn sh");
        self.stdins
            .lock()
            .await
            .insert(channel, child.stdin.take().expect("stdin"));
        session.channel_success(channel)?;

        let handle = session.handle();
        tokio::spawn(async move {
            let out = child.wait_with_output().await.expect("wait sh");
            let _ = handle.data(channel, out.stdout).await;
            let _ = handle.extended_data(channel, 1, out.stderr).await;
            let code = out.status.code().unwrap_or(255) as u32;
            let _ = handle.exit_status_request(channel, code).await;
            let _ = handle.eof(channel).await;
            let _ = handle.close(channel).await;
        });
        Ok(())
    }

    async fn data(
        &mut self,
        channel: ChannelId,
        data: &[u8],
        _session: &mut Session,
    ) -> Result<(), Self::Error> {
        if let Some(stdin) = self.stdins.lock().await.get_mut(&channel) {
            let _ = stdin.write_all(data).await;
        }
        Ok(())
    }

    async fn channel_eof(
        &mut self,
        channel: ChannelId,
        _session: &mut Session,
    ) -> Result<(), Self::Error> {
        self.stdins.lock().await.remove(&channel);
        Ok(())
    }
}

/// Starts a server on a free localhost port and returns the port.
pub async fn start(login: Login, prelude: &str) -> u16 {
    start_with(login, prelude, None).await
}

/// Like [`start`], also accepting keys listed in `authorized_keys`.
pub async fn start_with(login: Login, prelude: &str, authorized_keys: Option<PathBuf>) -> u16 {
    let key = decode_secret_key(include_str!("../fixtures/host_ed25519"), None).unwrap();
    let config = Arc::new(server::Config {
        keys: vec![key],
        auth_rejection_time: std::time::Duration::from_millis(10),
        auth_rejection_time_initial: Some(std::time::Duration::ZERO),
        ..Default::default()
    });
    let socket = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let port = socket.local_addr().unwrap().port();
    let mut server = TestServer {
        login,
        authorized_keys: authorized_keys.map(Arc::new),
        prelude: Arc::new(prelude.to_string()),
        stdins: Arc::default(),
    };
    tokio::spawn(async move {
        let _ = server.run_on_socket(config, &socket).await;
    });
    port
}
