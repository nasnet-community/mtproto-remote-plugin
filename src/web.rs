//! HTTP API and the embedded page.

use std::io::Write;
use std::sync::Arc;

use axum::body::Bytes;
use axum::extract::{Path, Query, State};
use axum::http::header::{ACCEPT_ENCODING, CACHE_CONTROL, CONTENT_ENCODING, CONTENT_TYPE, VARY};
use axum::http::{HeaderMap, Method, StatusCode};
use axum::response::{IntoResponse, Response};
use axum::routing::{any, get, post};
use axum::{Json, Router};
use flate2::Compression;
use flate2::write::GzEncoder;
use serde::{Deserialize, Serialize};
use serde_json::json;

use crate::deploy::{Deployer, Job, Request, Tracker, validate_host};
use crate::hostkeys::KeyStatus;
use crate::keys::authorized_line;
use crate::manage::{ManageError, Manager};
use crate::servers::ServerView;
use crate::ssh::{self, SshError};

const INDEX_HTML: &str = include_str!(concat!(env!("OUT_DIR"), "/index.html"));

pub struct AppState {
    pub deployer: Arc<Deployer>,
    pub manager: Arc<Manager>,
    pub tracker: Tracker,
    index: String,
    index_gz: Vec<u8>,
}

impl AppState {
    pub fn new(deployer: Deployer, manager: Manager, base_path: &str) -> Arc<Self> {
        let index = render_index(INDEX_HTML, base_path);
        let mut gz = GzEncoder::new(Vec::new(), Compression::best());
        gz.write_all(index.as_bytes()).expect("gzip to memory");
        Arc::new(Self {
            deployer: Arc::new(deployer),
            manager: Arc::new(manager),
            tracker: Tracker::default(),
            index,
            index_gz: gz.finish().expect("gzip to memory"),
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
        .route("/api/servers", get(list_servers))
        .route(
            "/api/servers/{id}",
            axum::routing::patch(rename_server).delete(remove_server),
        )
        .route("/api/servers/{id}/status", get(server_status))
        .route("/api/servers/{id}/service/{action}", post(service_action))
        .route("/api/servers/{id}/logs", get(server_logs))
        .route("/api/servers/{id}/update", post(update_server))
        .route("/api/servers/{id}/telemt/{*path}", any(telemt))
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

impl IntoResponse for ManageError {
    fn into_response(self) -> Response {
        let status = match &self {
            ManageError::NotFound => StatusCode::NOT_FOUND,
            ManageError::Forbidden => StatusCode::FORBIDDEN,
            ManageError::Ssh(SshError::HostKeyMismatch { .. }) => StatusCode::CONFLICT,
            _ => StatusCode::BAD_GATEWAY,
        };
        error(status, self.to_string())
    }
}

async fn index(State(state): State<Arc<AppState>>, headers: HeaderMap) -> Response {
    let gzip = headers
        .get(ACCEPT_ENCODING)
        .and_then(|v| v.to_str().ok())
        .is_some_and(|v| v.contains("gzip"));
    let common = [
        (CONTENT_TYPE, "text/html; charset=utf-8"),
        (CACHE_CONTROL, "no-store"),
        (VARY, "Accept-Encoding"),
    ];
    if gzip {
        (common, [(CONTENT_ENCODING, "gzip")], state.index_gz.clone()).into_response()
    } else {
        (common, state.index.clone()).into_response()
    }
}

async fn config(State(state): State<Arc<AppState>>) -> Response {
    let target = &state.deployer.target;
    Json(json!({
        "version": env!("CARGO_PKG_VERSION"),
        "repo": target.repo,
        "releaseTag": target.release_tag,
        "routerKey": authorized_line(&state.deployer.key),
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
    req.normalize();
    if let Err(message) = req.validate() {
        return error(StatusCode::BAD_REQUEST, message);
    }
    let Some(id) = state.tracker.start() else {
        return error(StatusCode::CONFLICT, "An installation is already running.");
    };
    let (deployer, tracker) = (state.deployer.clone(), state.tracker.clone());
    tokio::spawn(async move { deployer.run(req, &tracker).await });
    (StatusCode::ACCEPTED, Json(json!({ "id": id }))).into_response()
}

async fn job(State(state): State<Arc<AppState>>) -> Json<Job> {
    Json(state.tracker.snapshot())
}

async fn list_servers(State(state): State<Arc<AppState>>) -> Json<Vec<ServerView>> {
    Json(
        state
            .manager
            .store
            .list()
            .iter()
            .map(ServerView::from)
            .collect(),
    )
}

#[derive(Deserialize)]
struct Rename {
    name: String,
}

async fn rename_server(
    State(state): State<Arc<AppState>>,
    Path(id): Path<String>,
    Json(body): Json<Rename>,
) -> Response {
    let name = body.name.trim();
    if name.is_empty() || name.chars().count() > 64 {
        return error(StatusCode::BAD_REQUEST, "Use a name of 1 to 64 characters.");
    }
    match state.manager.store.rename(&id, name) {
        Ok(Some(server)) => Json(ServerView::from(&server)).into_response(),
        Ok(None) => ManageError::NotFound.into_response(),
        Err(e) => error(StatusCode::INTERNAL_SERVER_ERROR, e.to_string()),
    }
}

#[derive(Deserialize, Default)]
struct Remove {
    #[serde(default)]
    uninstall: bool,
}

async fn remove_server(
    State(state): State<Arc<AppState>>,
    Path(id): Path<String>,
    body: Option<Json<Remove>>,
) -> Response {
    let uninstall = body.unwrap_or_default().uninstall;
    match state.manager.remove(&id, uninstall).await {
        Ok(warning) => Json(json!({ "removed": true, "warning": warning })).into_response(),
        Err(e) => e.into_response(),
    }
}

async fn server_status(State(state): State<Arc<AppState>>, Path(id): Path<String>) -> Response {
    match state.manager.status(&id).await {
        Ok(status) => Json(status).into_response(),
        Err(e) => e.into_response(),
    }
}

async fn service_action(
    State(state): State<Arc<AppState>>,
    Path((id, action)): Path<(String, String)>,
) -> Response {
    match state.manager.service(&id, &action).await {
        Ok(()) => Json(json!({ "ok": true })).into_response(),
        Err(e) => e.into_response(),
    }
}

#[derive(Deserialize)]
struct LogsQuery {
    lines: Option<u32>,
}

async fn server_logs(
    State(state): State<Arc<AppState>>,
    Path(id): Path<String>,
    Query(q): Query<LogsQuery>,
) -> Response {
    match state.manager.logs(&id, q.lines.unwrap_or(200)).await {
        Ok(text) => Json(json!({ "logs": text })).into_response(),
        Err(e) => e.into_response(),
    }
}

async fn update_server(State(state): State<Arc<AppState>>, Path(id): Path<String>) -> Response {
    match state.manager.update(&id).await {
        Ok(release) => Json(json!({ "release": release })).into_response(),
        Err(e) => e.into_response(),
    }
}

/// Passes an allowed request through to telemt's API and returns its reply as is.
async fn telemt(
    State(state): State<Arc<AppState>>,
    Path((id, path)): Path<(String, String)>,
    method: Method,
    body: Bytes,
) -> Response {
    let body = (!body.is_empty()).then_some(body.as_ref());
    match state
        .manager
        .telemt(&id, method.as_str(), &path, body)
        .await
    {
        Ok(res) => (
            StatusCode::from_u16(res.status).unwrap_or(StatusCode::BAD_GATEWAY),
            [(CONTENT_TYPE, "application/json; charset=utf-8")],
            res.body,
        )
            .into_response(),
        Err(e) => e.into_response(),
    }
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
    fn embedded_page_has_a_head_for_the_base_href() {
        assert!(INDEX_HTML.contains("<head>"));
    }
}
