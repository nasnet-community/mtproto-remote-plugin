//! HTTP API and the embedded page.

use std::sync::Arc;

use axum::extract::State;
use axum::http::StatusCode;
use axum::http::header::{CACHE_CONTROL, CONTENT_TYPE};
use axum::response::{IntoResponse, Response};
use axum::routing::{get, post};
use axum::{Json, Router};
use serde::{Deserialize, Serialize};
use serde_json::json;

use crate::deploy::{Deployer, Job, Request, Tracker, validate_host};
use crate::hostkeys::KeyStatus;
use crate::ssh;

const INDEX_HTML: &str = include_str!("index.html");

pub struct AppState {
    pub deployer: Arc<Deployer>,
    pub tracker: Tracker,
    index: String,
}

impl AppState {
    pub fn new(deployer: Deployer, base_path: &str) -> Arc<Self> {
        Arc::new(Self {
            deployer: Arc::new(deployer),
            tracker: Tracker::default(),
            index: render_index(INDEX_HTML, base_path),
        })
    }
}

/// Serves the app at `base_path` and also at the root, so it works both
/// behind a proxy that keeps the prefix and one that strips it.
pub fn router(state: Arc<AppState>, base_path: &str) -> Router {
    let routes = Router::new()
        .route("/", get(index))
        .route("/health", get(|| async { "ok" }))
        .route("/api/config", get(config))
        .route("/api/probe", post(probe))
        .route("/api/deploy", post(deploy))
        .route("/api/job", get(job))
        .with_state(state.clone());

    if base_path.is_empty() {
        return routes;
    }
    Router::new()
        .route(&format!("{base_path}/"), get(index).with_state(state))
        .nest(base_path, routes.clone())
        .merge(routes)
}

fn render_index(html: &str, base_path: &str) -> String {
    html.replacen(
        "<head>",
        &format!("<head>\n    <base href=\"{base_path}/\" />"),
        1,
    )
}

fn error(status: StatusCode, message: impl Into<String>) -> Response {
    (status, Json(json!({ "error": message.into() }))).into_response()
}

async fn index(State(state): State<Arc<AppState>>) -> Response {
    (
        [
            (CONTENT_TYPE, "text/html; charset=utf-8"),
            (CACHE_CONTROL, "no-store"),
        ],
        state.index.clone(),
    )
        .into_response()
}

async fn config(State(state): State<Arc<AppState>>) -> Response {
    let t = &state.deployer.target;
    Json(json!({
        "version": env!("CARGO_PKG_VERSION"),
        "repo": t.repo,
        "releaseTag": t.release_tag,
        "binaryName": t.binary_name,
        "serviceName": t.service_name,
        "execArgs": t.exec_args,
        "needsPort": t.exec_args.contains("{port}"),
        "needsSecret": t.exec_args.contains("{secret}"),
    }))
    .into_response()
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct ProbeRequest {
    host: String,
    ssh_port: u16,
}

#[derive(Serialize)]
struct ProbeResponse {
    fingerprint: String,
    status: KeyStatus,
}

async fn probe(State(state): State<Arc<AppState>>, Json(req): Json<ProbeRequest>) -> Response {
    let host = req.host.trim();
    if let Err(message) = validate_host(host) {
        return error(StatusCode::BAD_REQUEST, message);
    }
    if req.ssh_port == 0 {
        return error(StatusCode::BAD_REQUEST, "Enter the SSH port.");
    }
    match ssh::probe(host, req.ssh_port).await {
        Ok(fingerprint) => {
            let status = state
                .deployer
                .host_keys
                .status(host, req.ssh_port, &fingerprint);
            Json(ProbeResponse {
                fingerprint,
                status,
            })
            .into_response()
        }
        Err(e) => error(StatusCode::BAD_GATEWAY, e.to_string()),
    }
}

async fn deploy(State(state): State<Arc<AppState>>, Json(mut req): Json<Request>) -> Response {
    req.host = req.host.trim().to_string();
    req.username = req.username.trim().to_string();
    req.secret = req.secret.trim().to_string();
    if let Err(message) = req.validate(&state.deployer.target) {
        return error(StatusCode::BAD_REQUEST, message);
    }
    let Some(id) = state.tracker.start() else {
        return error(StatusCode::CONFLICT, "A deployment is already running.");
    };
    let (deployer, tracker) = (state.deployer.clone(), state.tracker.clone());
    tokio::spawn(async move { deployer.run(req, &tracker).await });
    (StatusCode::ACCEPTED, Json(json!({ "id": id }))).into_response()
}

async fn job(State(state): State<Arc<AppState>>) -> Json<Job> {
    Json(state.tracker.snapshot())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn injects_base_href_with_trailing_slash() {
        let html = render_index("<html><head><title>x</title></head></html>", "/a/b");
        assert!(html.contains("<head>\n    <base href=\"/a/b/\" />"));
        assert!(render_index("<head>", "").contains("<base href=\"/\" />"));
    }

    #[test]
    fn page_uses_only_relative_api_urls() {
        assert!(INDEX_HTML.contains("<head>"));
        assert!(!INDEX_HTML.contains("fetch(\"/"));
        assert!(!INDEX_HTML.contains("fetch('/"));
    }
}
