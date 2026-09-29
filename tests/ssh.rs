mod common;

use common::{HOST_FINGERPRINT, Login, PASSWORD, USER};
use mtproto_remote::ssh::{Session, SshError, probe};

#[tokio::test]
async fn probe_reports_the_host_key_fingerprint() {
    let port = common::start(Login::Password, "").await;
    assert_eq!(probe("127.0.0.1", port).await.unwrap(), HOST_FINGERPRINT);
}

#[tokio::test]
async fn probe_fails_cleanly_when_nothing_listens() {
    let port = {
        let l = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        l.local_addr().unwrap().port()
    };
    assert!(matches!(
        probe("127.0.0.1", port).await,
        Err(SshError::Connect { .. })
    ));
}

#[tokio::test]
async fn exec_returns_output_exit_code_and_takes_stdin() {
    let port = common::start(Login::Password, "").await;
    let s = Session::connect("127.0.0.1", port, HOST_FINGERPRINT, USER, PASSWORD)
        .await
        .unwrap();

    let out = s.exec("echo out; echo err >&2; exit 3", b"").await.unwrap();
    assert_eq!(out.code, Some(3));
    assert_eq!(out.stdout, "out\n");
    assert_eq!(out.stderr, "err\n");

    // Larger than one SSH window, to exercise flow control on upload.
    let payload: Vec<u8> = (0..5_000_000u32).map(|i| (i % 251) as u8).collect();
    let out = s.exec("wc -c", &payload).await.unwrap();
    assert!(out.success());
    assert_eq!(out.stdout.trim(), payload.len().to_string());
    s.close().await;
}

#[tokio::test]
async fn refuses_a_host_key_other_than_the_confirmed_one() {
    let port = common::start(Login::Password, "").await;
    let err = Session::connect("127.0.0.1", port, "SHA256:someone-else", USER, PASSWORD)
        .await
        .err()
        .unwrap();
    assert!(
        matches!(&err, SshError::HostKeyMismatch { actual, .. } if actual == HOST_FINGERPRINT),
        "{err}"
    );
}

#[tokio::test]
async fn rejects_a_wrong_password() {
    let port = common::start(Login::Password, "").await;
    let err = Session::connect("127.0.0.1", port, HOST_FINGERPRINT, USER, "nope")
        .await
        .err()
        .unwrap();
    assert!(matches!(err, SshError::AuthFailed(_)), "{err}");
}

#[tokio::test]
async fn falls_back_to_keyboard_interactive() {
    let port = common::start(Login::KeyboardInteractive, "").await;
    let s = Session::connect("127.0.0.1", port, HOST_FINGERPRINT, USER, PASSWORD)
        .await
        .unwrap();
    assert!(s.exec("true", b"").await.unwrap().success());
}

fn temp_dir(name: &str) -> std::path::PathBuf {
    let dir = std::env::temp_dir().join(format!("ssh-test-{name}-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    dir
}

#[tokio::test]
async fn logs_in_with_a_listed_key_and_not_with_others() {
    let dir = temp_dir("keys");
    let ours = mtproto_remote::keys::load_or_create(&dir.join("id_ed25519")).unwrap();
    let theirs = mtproto_remote::keys::load_or_create(&dir.join("other")).unwrap();
    let listed = dir.join("authorized_keys");
    std::fs::write(&listed, mtproto_remote::keys::authorized_line(&ours) + "\n").unwrap();
    let port = common::start_with(Login::Password, "", Some(listed)).await;

    let s = Session::connect_with_key("127.0.0.1", port, HOST_FINGERPRINT, USER, &ours)
        .await
        .unwrap();
    assert!(s.exec("true", b"").await.unwrap().success());

    let err = Session::connect_with_key("127.0.0.1", port, HOST_FINGERPRINT, USER, &theirs)
        .await
        .err()
        .unwrap();
    assert!(matches!(err, SshError::KeyRejected(_)), "{err}");
    std::fs::remove_dir_all(dir).unwrap();
}

#[tokio::test]
async fn sends_http_to_a_loopback_port_through_the_connection() {
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let api_port = listener.local_addr().unwrap().port();
    tokio::spawn(async move {
        let (mut conn, _) = listener.accept().await.unwrap();
        let mut buf = vec![0u8; 4096];
        let n = conn.read(&mut buf).await.unwrap();
        let request = String::from_utf8_lossy(&buf[..n]).to_string();
        let body = format!("{{\"line\":{:?}}}", request.lines().next().unwrap());
        let reply = format!(
            "HTTP/1.1 200 OK\r\ncontent-length: {}\r\n\r\n{body}",
            body.len()
        );
        conn.write_all(reply.as_bytes()).await.unwrap();
    });

    let port = common::start(Login::Password, "").await;
    let s = Session::connect("127.0.0.1", port, HOST_FINGERPRINT, USER, PASSWORD)
        .await
        .unwrap();
    let res = s
        .http(api_port, "GET", "/v1/users", Some("Bearer t"), None)
        .await
        .unwrap();
    assert_eq!(res.status, 200);
    assert_eq!(
        String::from_utf8(res.body).unwrap(),
        r#"{"line":"GET /v1/users HTTP/1.1"}"#
    );

    let closed = {
        let l = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        l.local_addr().unwrap().port()
    };
    let err = s.http(closed, "GET", "/", None, None).await.err().unwrap();
    assert!(matches!(err, SshError::Tunnel { .. }), "{err}");
}
