mod common;

use std::sync::Arc;
use std::time::Duration;

use common::{HOST_FINGERPRINT, Login};
use mtproto_remote::config::Target;
use mtproto_remote::deploy::{Deployer, GitHubSource};
use mtproto_remote::hostkeys::HostKeys;
use mtproto_remote::web;
use serde_json::{Value, json};

const BASE: &str = "/api/plugin/view/mtproto-remote";

async fn serve() -> String {
    let deployer = Deployer {
        target: Target::default(),
        source: Arc::new(GitHubSource::default()),
        host_keys: HostKeys::new(
            std::env::temp_dir().join(format!("web-test-{}/keys.json", std::process::id())),
        ),
        settle: Duration::ZERO,
    };
    let app = web::router(web::AppState::new(deployer, BASE), BASE);
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    tokio::spawn(async move { axum::serve(listener, app).await.unwrap() });
    format!("http://{addr}")
}

/// Blocking HTTP call on a blocking thread; returns (status, body).
async fn call(method: &'static str, url: String, body: Option<Value>) -> (u16, String) {
    tokio::task::spawn_blocking(move || {
        let agent: ureq::Agent = ureq::Agent::config_builder()
            .http_status_as_error(false)
            .build()
            .into();
        let mut res = match (method, body) {
            ("GET", _) => agent.get(&url).call(),
            (_, Some(body)) => agent.post(&url).send_json(body),
            _ => unreachable!(),
        }
        .unwrap();
        (
            res.status().as_u16(),
            res.body_mut().read_to_string().unwrap(),
        )
    })
    .await
    .unwrap()
}

#[tokio::test(flavor = "multi_thread")]
async fn serves_the_page_under_the_base_path_and_the_root() {
    let url = serve().await;
    for path in [format!("{BASE}/"), BASE.to_string(), "/".to_string()] {
        let (status, body) = call("GET", format!("{url}{path}"), None).await;
        assert_eq!(status, 200, "{path}");
        assert!(
            body.contains(&format!("<base href=\"{BASE}/\" />")),
            "{path}"
        );
    }
}

#[tokio::test(flavor = "multi_thread")]
async fn api_works_with_and_without_the_prefix() {
    let url = serve().await;
    for prefix in [BASE, ""] {
        let (status, body) = call("GET", format!("{url}{prefix}/api/config"), None).await;
        assert_eq!(status, 200);
        let cfg: Value = serde_json::from_str(&body).unwrap();
        assert_eq!(cfg["repo"], "9seconds/mtg");
        assert_eq!(cfg["needsSecret"], true);

        let (status, body) = call("GET", format!("{url}{prefix}/api/job"), None).await;
        assert_eq!(status, 200);
        assert_eq!(
            serde_json::from_str::<Value>(&body).unwrap()["running"],
            false
        );
    }
}

#[tokio::test(flavor = "multi_thread")]
async fn probe_returns_the_fingerprint_of_a_new_server() {
    let url = serve().await;
    let port = common::start(Login::Password, "").await;
    let (status, body) = call(
        "POST",
        format!("{url}{BASE}/api/probe"),
        Some(json!({ "host": "127.0.0.1", "sshPort": port })),
    )
    .await;
    assert_eq!(status, 200, "{body}");
    let key: Value = serde_json::from_str(&body).unwrap();
    assert_eq!(key["fingerprint"], HOST_FINGERPRINT);
    assert_eq!(key["status"], "new");
}

#[tokio::test(flavor = "multi_thread")]
async fn rejects_bad_input_with_a_message() {
    let url = serve().await;
    let (status, body) = call(
        "POST",
        format!("{url}/api/probe"),
        Some(json!({ "host": "bad host;", "sshPort": 22 })),
    )
    .await;
    assert_eq!(status, 400);
    assert!(body.contains("hostname"));

    let (status, body) = call(
        "POST",
        format!("{url}/api/deploy"),
        Some(json!({
            "host": "127.0.0.1", "sshPort": 22, "username": "root", "password": "x",
            "fingerprint": HOST_FINGERPRINT, "proxyPort": 443, "secret": "bad secret"
        })),
    )
    .await;
    assert_eq!(status, 400);
    assert!(body.contains("secret"), "{body}");
}
