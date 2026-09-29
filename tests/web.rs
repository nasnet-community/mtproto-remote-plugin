//! The HTTP API end to end: the page, installing a server, and managing it.

mod common;

use std::time::Duration;

use common::fake::{Fake, Options};
use common::{HOST_FINGERPRINT, PASSWORD, USER};
use mtproto_remote::web;
use serde_json::{Value, json};

const BASE: &str = "/api/plugin/view/mtproto-remote";

async fn serve(fake: &Fake) -> String {
    let state = web::AppState::new(fake.deployer(), fake.manager(), BASE);
    let app = web::router(state, BASE);
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    tokio::spawn(async move { axum::serve(listener, app).await.unwrap() });
    format!("http://{addr}")
}

/// Blocking HTTP call on a blocking thread; returns (status, content-encoding, body).
async fn call(
    method: &'static str,
    url: String,
    body: Option<Value>,
    gzip: bool,
) -> (u16, Option<String>, String) {
    tokio::task::spawn_blocking(move || {
        let agent: ureq::Agent = ureq::Agent::config_builder()
            .http_status_as_error(false)
            .build()
            .into();
        let mut res = match (method, body) {
            ("GET", _) => {
                let req = agent.get(&url);
                if gzip {
                    req.header("Accept-Encoding", "gzip")
                } else {
                    req
                }
                .call()
            }
            ("DELETE", None) => agent.delete(&url).call(),
            ("DELETE", Some(b)) => agent.delete(&url).force_send_body().send_json(b),
            ("POST", None) => agent.post(&url).send_empty(),
            ("POST", Some(b)) => agent.post(&url).send_json(b),
            ("PATCH", Some(b)) => agent.patch(&url).send_json(b),
            _ => unreachable!(),
        }
        .unwrap();
        let encoding = res
            .headers()
            .get("content-encoding")
            .map(|v| v.to_str().unwrap().to_string());
        let body = if encoding.is_some() {
            String::new()
        } else {
            res.body_mut().read_to_string().unwrap()
        };
        (res.status().as_u16(), encoding, body)
    })
    .await
    .unwrap()
}

async fn send(method: &'static str, url: String, body: Option<Value>) -> (u16, Value) {
    let (status, _, body) = call(method, url, body, false).await;
    (status, serde_json::from_str(&body).unwrap_or(Value::Null))
}

async fn get(url: String) -> (u16, Value) {
    send("GET", url, None).await
}

#[tokio::test(flavor = "multi_thread")]
async fn serves_the_page_under_the_base_path_and_the_root() {
    let fake = Fake::new("web-page", Options::default()).await;
    let url = serve(&fake).await;
    for path in [format!("{BASE}/"), BASE.to_string(), "/".to_string()] {
        let (status, _, body) = call("GET", format!("{url}{path}"), None, false).await;
        assert_eq!(status, 200, "{path}");
        assert!(
            body.contains(&format!("<base href=\"{BASE}/\" />")),
            "{path}"
        );
    }
    let (status, encoding, _) = call("GET", format!("{url}/"), None, true).await;
    assert_eq!((status, encoding.as_deref()), (200, Some("gzip")));
}

#[tokio::test(flavor = "multi_thread")]
async fn config_shows_the_router_key() {
    let fake = Fake::new("web-config", Options::default()).await;
    let url = serve(&fake).await;
    let (status, cfg) = get(format!("{url}{BASE}/api/config")).await;
    assert_eq!(status, 200);
    assert_eq!(cfg["repo"], "telemt/telemt");
    let key = cfg["routerKey"].as_str().unwrap();
    assert!(key.starts_with("ssh-ed25519 ") && key.ends_with(" nasnet-mtproto-remote"));
}

#[tokio::test(flavor = "multi_thread")]
async fn rejects_bad_input_with_a_message() {
    let fake = Fake::new("web-bad", Options::default()).await;
    let url = serve(&fake).await;
    let (status, body) = send(
        "POST",
        format!("{url}/api/probe"),
        Some(json!({ "host": "bad host;", "sshPort": 22 })),
    )
    .await;
    assert_eq!(status, 400);
    assert!(body["error"].as_str().unwrap().contains("hostname"));

    let mut req = json!({
        "host": "127.0.0.1", "sshPort": 22, "username": "root", "password": "x",
        "fingerprint": HOST_FINGERPRINT, "proxyPort": 443, "tlsDomain": "www.google.com",
        "firstUser": "bad user"
    });
    let (status, body) = send("POST", format!("{url}/api/deploy"), Some(req.clone())).await;
    assert_eq!(status, 400);
    assert!(
        body["error"].as_str().unwrap().contains("first user"),
        "{body}"
    );
    req["firstUser"] = json!("ok");
    req["tlsDomain"] = json!("nodot");
    let (status, _) = send("POST", format!("{url}/api/deploy"), Some(req)).await;
    assert_eq!(status, 400);
}

#[tokio::test(flavor = "multi_thread")]
async fn installs_then_manages_a_server_over_http() {
    let fake = Fake::new("web-flow", Options::default()).await;
    let url = serve(&fake).await;

    let (status, key) = send(
        "POST",
        format!("{url}/api/probe"),
        Some(json!({ "host": "127.0.0.1", "sshPort": fake.ssh_port })),
    )
    .await;
    assert_eq!(status, 200);
    assert_eq!(key["fingerprint"], HOST_FINGERPRINT);
    assert_eq!(key["status"], "new");

    let (status, _) = send(
        "POST",
        format!("{url}/api/deploy"),
        Some(json!({
            "host": " 127.0.0.1 ", "sshPort": fake.ssh_port, "username": USER,
            "password": PASSWORD, "fingerprint": HOST_FINGERPRINT, "name": "",
            "proxyPort": 443, "tlsDomain": "www.google.com", "firstUser": "first"
        })),
    )
    .await;
    assert_eq!(status, 202);
    let job = loop {
        let (_, job) = get(format!("{url}/api/job")).await;
        if job["running"] == false {
            break job;
        }
        tokio::time::sleep(Duration::from_millis(100)).await;
    };
    assert_eq!(job["error"], Value::Null, "{job}");
    let id = job["report"]["serverId"].as_str().unwrap().to_string();

    let (_, servers) = get(format!("{url}/api/servers")).await;
    assert_eq!(servers[0]["id"], id.as_str());
    assert_eq!(
        servers[0]["name"], "127.0.0.1",
        "an empty name defaults to the host"
    );
    assert!(
        servers[0].get("apiToken").is_none(),
        "the token must not reach the page"
    );

    let srv = format!("{url}{BASE}/api/servers/{id}");
    let (status, created) = send(
        "POST",
        format!("{srv}/telemt/v1/users"),
        Some(json!({ "username": "alice" })),
    )
    .await;
    assert_eq!(status, 201);
    assert_eq!(created["ok"], true);
    let (status, users) = get(format!("{srv}/telemt/v1/users")).await;
    assert_eq!(status, 200);
    assert_eq!(users["data"].as_array().unwrap().len(), 2);
    let (status, _) = send("POST", format!("{srv}/telemt/v1/system/reload"), None).await;
    assert_eq!(status, 403);

    let (status, st) = get(format!("{srv}/status")).await;
    assert_eq!((status, st["service"].as_str()), (200, Some("active")));
    let (status, _) = send("POST", format!("{srv}/service/restart"), None).await;
    assert_eq!(status, 200);
    let (status, logs) = get(format!("{srv}/logs?lines=5")).await;
    assert_eq!(status, 200);
    assert!(logs["logs"].as_str().unwrap().contains("-n 5"));

    let (status, renamed) = send("PATCH", srv.clone(), Some(json!({ "name": "Helsinki" }))).await;
    assert_eq!((status, renamed["name"].as_str()), (200, Some("Helsinki")));

    let (status, removed) = send("DELETE", srv.clone(), Some(json!({ "uninstall": true }))).await;
    assert_eq!(status, 200, "{removed}");
    assert_eq!(removed["warning"], Value::Null);
    let (status, _) = get(format!("{srv}/status")).await;
    assert_eq!(status, 404);
    assert!(!fake.exists("/usr/local/bin/telemt"));
}
