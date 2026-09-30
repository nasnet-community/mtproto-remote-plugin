//! Managing an installed server over the router key, against the fake server.

mod common;

use common::fake::{Fake, Options};
use mtproto_remote::deploy::Tracker;
use mtproto_remote::keys::authorized_line;
use mtproto_remote::manage::{ManageError, Manager};
use mtproto_remote::ssh::SshError;
use serde_json::Value;

/// Installs on a fresh fake server and returns it with a manager and server id.
async fn installed(name: &str, opts: Options) -> (Fake, Manager, String) {
    let fake = Fake::new(name, opts).await;
    let tracker = Tracker::default();
    tracker.start().unwrap();
    fake.deployer().run(fake.request(), &tracker).await;
    let job = tracker.snapshot();
    assert_eq!(job.error, None, "{job:#?}");
    let id = job.report.unwrap().server_id;
    let manager = fake.manager();
    (fake, manager, id)
}

fn json(body: &[u8]) -> Value {
    serde_json::from_slice(body).unwrap()
}

async fn usernames(m: &Manager, id: &str) -> Vec<String> {
    let res = m.telemt(id, "GET", "v1/users", None).await.unwrap();
    json(&res.body)["data"]
        .as_array()
        .unwrap()
        .iter()
        .map(|u| u["username"].as_str().unwrap().to_string())
        .collect()
}

#[tokio::test]
async fn manages_users_through_telemt_api_over_ssh() {
    let (_fake, m, id) = installed("users", Options::default()).await;
    assert_eq!(usernames(&m, &id).await, ["first"]);

    let res = m
        .telemt(&id, "POST", "v1/users", Some(br#"{"username":"alice"}"#))
        .await
        .unwrap();
    assert_eq!(res.status, 201);
    let dup = m
        .telemt(&id, "POST", "v1/users", Some(br#"{"username":"alice"}"#))
        .await
        .unwrap();
    assert_eq!(dup.status, 409, "telemt's own errors pass through");
    assert_eq!(usernames(&m, &id).await, ["first", "alice"]);

    let res = m
        .telemt(&id, "DELETE", "v1/users/alice", None)
        .await
        .unwrap();
    assert_eq!(res.status, 202);
    assert_eq!(usernames(&m, &id).await, ["first"]);

    assert!(matches!(
        m.telemt(&id, "POST", "v1/system/reload", None).await,
        Err(ManageError::Forbidden)
    ));
    assert!(matches!(
        m.telemt("nope", "GET", "v1/users", None).await,
        Err(ManageError::NotFound)
    ));
}

#[tokio::test]
async fn reports_status_and_controls_the_service_through_the_helper() {
    let (fake, m, id) = installed("service", Options::default()).await;
    let status = m.status(&id).await.unwrap();
    assert_eq!(status.service, "active");
    assert_eq!(status.info.unwrap()["version"], "3.5.9");
    assert_eq!(status.api_error, None);

    m.service(&id, "stop").await.unwrap();
    assert_eq!(m.status(&id).await.unwrap().service, "inactive");
    m.service(&id, "start").await.unwrap();
    assert_eq!(m.status(&id).await.unwrap().service, "active");
    assert!(matches!(
        m.service(&id, "reboot").await,
        Err(ManageError::Forbidden)
    ));
    let log = fake.read("/systemctl.log");
    assert!(log.lines().any(|l| l == "stop telemt") && log.lines().any(|l| l == "start telemt"));

    let logs = m.logs(&id, 50).await.unwrap();
    assert_eq!(
        logs.trim(),
        "fake log: -u telemt --no-pager -o short-iso -n 50"
    );
}

#[tokio::test]
async fn updates_the_binary_and_restarts() {
    let (fake, m, id) = installed("update", Options::default()).await;
    *fake.source.tag.lock().unwrap() = "3.6.0".into();
    assert_eq!(m.update(&id).await.unwrap(), "3.6.0");
    assert!(fake.read("/usr/local/bin/telemt").ends_with("3.6.0"));
    assert_eq!(
        fake.read("/systemctl.log").lines().last(),
        Some("restart telemt")
    );
}

#[tokio::test]
async fn uninstall_removes_telemt_and_the_router_key() {
    let (fake, m, id) = installed("uninstall", Options::default()).await;
    let line = authorized_line(&fake.key);
    assert!(fake.authorized_keys().contains(&line));

    assert_eq!(m.remove(&id, true).await.unwrap(), None);
    for path in [
        "/usr/local/bin/telemt",
        "/usr/local/sbin/nasnet-telemt-ctl",
        "/etc/telemt",
        "/var/lib/telemt",
        "/etc/systemd/system/telemt.service",
        "/etc/sudoers.d/nasnet-telemt",
    ] {
        assert!(!fake.exists(path), "{path} left behind");
    }
    assert_eq!(fake.read("/users"), "");
    assert!(!fake.authorized_keys().contains(&line));
    assert!(fake.store.list().is_empty());
}

#[tokio::test]
async fn uninstall_as_root_works_without_sudo() {
    let opts = Options {
        uid: 0,
        sudo_allowed: false,
        ..Options::default()
    };
    let (fake, m, id) = installed("uninstall-root", opts).await;
    assert_eq!(m.remove(&id, true).await.unwrap(), None);
    assert!(!fake.exists("/usr/local/bin/telemt"));
}

#[tokio::test]
async fn forgetting_an_unreachable_server_warns_but_removes_it() {
    let (fake, m, id) = installed("forget", Options::default()).await;
    let mut server = fake.store.get(&id).unwrap();
    server.ssh_port = std::net::TcpListener::bind("127.0.0.1:0")
        .unwrap()
        .local_addr()
        .unwrap()
        .port();
    fake.store.upsert(server).unwrap();

    let warning = m.remove(&id, false).await.unwrap().unwrap();
    assert!(warning.contains("could not be cleaned up"), "{warning}");
    assert!(fake.store.list().is_empty());
}

#[tokio::test]
async fn a_failed_uninstall_keeps_the_server_for_a_retry() {
    let (fake, m, id) = installed("uninstall-fail", Options::default()).await;
    std::fs::remove_file(format!("{}/etc/sudoers.d/nasnet-telemt", fake.layout.root)).unwrap();
    let err = m.remove(&id, true).await.err().unwrap();
    assert!(err.to_string().contains("a password is required"), "{err}");
    assert_eq!(fake.store.list().len(), 1);
}

#[tokio::test]
async fn says_so_when_the_router_key_was_revoked() {
    let (fake, _m, id) = installed("revoked", Options::default()).await;
    std::fs::write(fake.root.join("home/.ssh/authorized_keys"), "").unwrap();
    let m = fake.manager(); // no cached session
    let err = m.status(&id).await.err().unwrap();
    assert!(
        matches!(err, ManageError::Ssh(SshError::KeyRejected(_))),
        "{err}"
    );
}
