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
