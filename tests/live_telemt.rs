//! Runs the real telemt binary with the config this app writes, and manages it
//! through the SSH tunnel. Downloads telemt from GitHub, so it is opt-in:
//! `cargo test --test live_telemt -- --ignored`.

mod common;

use std::process::Stdio;
use std::time::Duration;

use common::{HOST_FINGERPRINT, Login, PASSWORD, USER};
use mtproto_remote::config::Target;
use mtproto_remote::deploy::{GitHubSource, Source};
use mtproto_remote::ssh::Session;
use mtproto_remote::telemt::{Layout, NewConfig, config_toml};
use serde_json::{Value, json};

fn free_port() -> u16 {
    std::net::TcpListener::bind("127.0.0.1:0")
        .unwrap()
        .local_addr()
        .unwrap()
        .port()
}

#[tokio::test(flavor = "multi_thread")]
#[ignore]
async fn real_telemt_accepts_our_config_and_api_calls() {
    let dir = std::env::temp_dir().join(format!("live-telemt-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();

    let fetched =
        tokio::task::spawn_blocking(|| GitHubSource::default().fetch(&Target::default(), "x86_64"))
            .await
            .unwrap()
            .unwrap();
    let bin = dir.join("telemt");
    std::fs::write(&bin, &fetched.binary).unwrap();
    use std::os::unix::fs::PermissionsExt;
    std::fs::set_permissions(&bin, std::fs::Permissions::from_mode(0o755)).unwrap();

    let layout = Layout {
        root: String::new(),
        api_port: free_port(),
    };
    let proxy_port = free_port();
    let token = "livetoken0123456789abcdef";
    let conf = dir.join("telemt.toml");
    std::fs::write(
        &conf,
        config_toml(
            &layout,
            &NewConfig {
                public_host: "203.0.113.10".into(),
                port: proxy_port,
                tls_domain: "www.google.com".into(),
                first_user: "first".into(),
                first_secret: "0123456789abcdef0123456789abcdef".into(),
                api_token: token.into(),
            },
        ),
    )
    .unwrap();
    let mut child = tokio::process::Command::new(&bin)
        .arg("run")
        .arg(&conf)
        .current_dir(&dir)
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .kill_on_drop(true)
        .spawn()
        .unwrap();

    let port = common::start(Login::Password, "").await;
    let s = Session::connect("127.0.0.1", port, HOST_FINGERPRINT, USER, PASSWORD)
        .await
        .unwrap();
    let auth = format!("Bearer {token}");
    let call = |method: &'static str, path: String, body: Option<Value>| {
        let (s, auth) = (&s, auth.clone());
        async move {
            let body = body.map(|b| b.to_string());
            let res = s
                .http(
                    layout.api_port,
                    method,
                    &path,
                    Some(&auth),
                    body.as_deref().map(str::as_bytes),
                )
                .await
                .unwrap();
            (
                res.status,
                serde_json::from_slice::<Value>(&res.body).unwrap(),
            )
        }
    };

    // Wait for the API.
    let mut users = Value::Null;
    for _ in 0..40 {
        if let Ok(res) = s
            .http(layout.api_port, "GET", "/v1/users", Some(&auth), None)
            .await
            && res.status == 200
        {
            users = serde_json::from_slice(&res.body).unwrap();
            break;
        }
        tokio::time::sleep(Duration::from_millis(250)).await;
    }
    let first = &users["data"][0];
    assert_eq!(first["username"], "first", "{users}");
    let link = first["links"]["tls"][0].as_str().unwrap();
    assert!(
        link.starts_with(&format!(
            "tg://proxy?server=203.0.113.10&port={proxy_port}&secret=ee0123456789abcdef"
        )),
        "{link}"
    );

    let (status, _) = s
        .http(
            layout.api_port,
            "GET",
            "/v1/users",
            Some("Bearer wrong"),
            None,
        )
        .await
        .map(|r| (r.status, ()))
        .unwrap();
    assert_eq!(status, 401);

    let (status, created) = call(
        "POST",
        "/v1/users".into(),
        Some(json!({
            "username": "alice", "max_tcp_conns": 10, "data_quota_bytes": 1_000_000_000u64,
            "expiration_rfc3339": "2030-01-01T00:00:00Z", "max_unique_ips": 3
        })),
    )
    .await;
    // 201, or 202 while the hot reload that applies it is pending.
    assert!(status == 201 || status == 202, "{created}");
    assert_eq!(created["data"]["secret"].as_str().unwrap().len(), 32);

    let (status, patched) = call(
        "PATCH",
        "/v1/users/alice".into(),
        Some(json!({ "max_tcp_conns": null, "rate_limit_down_bps": 8_000_000 })),
    )
    .await;
    assert_eq!(status, 200, "{patched}");
    assert_eq!(patched["data"]["max_tcp_conns"], Value::Null);

    for action in ["disable", "enable", "rotate-secret", "reset-quota"] {
        let (status, body) = call("POST", format!("/v1/users/alice/{action}"), None).await;
        assert_eq!(status, 200, "{action}: {body}");
    }
    let (status, body) = call("DELETE", "/v1/users/alice".into(), None).await;
    assert!(status == 200 || status == 202, "{body}");

    let (status, body) = call(
        "PATCH",
        "/v1/config".into(),
        Some(json!({ "censorship": { "tls_domain": "www.cloudflare.com" } })),
    )
    .await;
    assert_eq!(status, 200, "{body}");
    let (status, info) = call("GET", "/v1/system/info".into(), None).await;
    assert_eq!(status, 200);
    assert_eq!(info["data"]["version"], fetched.tag.trim_start_matches('v'));

    child.kill().await.unwrap();
    std::fs::remove_dir_all(dir).unwrap();
}
