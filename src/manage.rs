//! Managing installed servers over the router's key: telemt's API through the
//! SSH connection, and service actions through the root helper.

use std::collections::HashMap;
use std::sync::Arc;

use russh::keys::PrivateKey;
use serde::Serialize;
use serde_json::Value;
use thiserror::Error;
use tokio::sync::Mutex;

use crate::config::Target;
use crate::deploy::Source;
use crate::http;
use crate::keys::authorized_line;
use crate::release::ReleaseError;
use crate::servers::{Server, ServerStore};
use crate::ssh::{Session, SshError};
use crate::telemt::{Layout, Remote, RemoteError, is_simple_name};

#[derive(Debug, Error)]
pub enum ManageError {
    #[error("no such server")]
    NotFound,
    #[error("that proxy API route is not allowed")]
    Forbidden,
    #[error(transparent)]
    Ssh(#[from] SshError),
    #[error(transparent)]
    Remote(#[from] RemoteError),
    #[error(transparent)]
    Release(#[from] ReleaseError),
    #[error("{0}")]
    Other(String),
}

/// True for the telemt API calls the page may make. `path` has no leading slash.
pub fn telemt_route_allowed(method: &str, path: &str) -> bool {
    let segments: Vec<&str> = path.split('/').collect();
    let user = |name: &str| is_simple_name(name, 64);
    match (method, segments.as_slice()) {
        ("GET" | "POST", ["v1", "users"]) => true,
        ("GET" | "PATCH" | "DELETE", ["v1", "users", name]) => user(name),
        ("POST", ["v1", "users", name, action]) => {
            user(name)
                && matches!(
                    *action,
                    "rotate-secret" | "enable" | "disable" | "reset-quota"
                )
        }
        ("GET", ["v1", "system", "info"] | ["v1", "stats", "summary"] | ["v1", "health"]) => true,
        ("GET" | "PATCH", ["v1", "config"]) => true,
        _ => false,
    }
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct Status {
    /// `systemctl is-active telemt`: active, inactive, failed, …
    pub service: String,
    /// telemt's `/v1/system/info` data, when its API answers.
    pub info: Option<Value>,
    pub summary: Option<Value>,
    pub api_error: Option<String>,
}

pub struct Manager {
    pub store: Arc<ServerStore>,
    pub key: Arc<PrivateKey>,
    pub layout: Layout,
    pub target: Target,
    pub source: Arc<dyn Source>,
    sessions: Mutex<HashMap<String, Arc<Session>>>,
}

impl Manager {
    pub fn new(
        store: Arc<ServerStore>,
        key: Arc<PrivateKey>,
        layout: Layout,
        target: Target,
        source: Arc<dyn Source>,
    ) -> Self {
        Self {
            store,
            key,
            layout,
            target,
            source,
            sessions: Mutex::default(),
        }
    }

    pub fn server(&self, id: &str) -> Result<Server, ManageError> {
        self.store.get(id).ok_or(ManageError::NotFound)
    }

    /// A cached key session for the server, reconnecting if it dropped.
    async fn session(&self, server: &Server) -> Result<Arc<Session>, ManageError> {
        let mut sessions = self.sessions.lock().await;
        if let Some(session) = sessions.get(&server.id).filter(|s| s.is_open()) {
            return Ok(session.clone());
        }
        let session = Arc::new(
            Session::connect_with_key(
                &server.host,
                server.ssh_port,
                &server.fingerprint,
                &server.username,
                &self.key,
            )
            .await?,
        );
        sessions.insert(server.id.clone(), session.clone());
        Ok(session)
    }

    async fn forget_session(&self, id: &str) {
        if let Some(session) = self.sessions.lock().await.remove(id) {
            session.close().await;
        }
    }

    /// Calls telemt's API on the server, retrying once on a fresh connection.
    pub async fn telemt(
        &self,
        id: &str,
        method: &str,
        path: &str,
        body: Option<&[u8]>,
    ) -> Result<http::Response, ManageError> {
        if !telemt_route_allowed(method, path) {
            return Err(ManageError::Forbidden);
        }
        let server = self.server(id)?;
        let auth = format!("Bearer {}", server.api_token);
        let path = format!("/{path}");
        for attempt in 0..2 {
            let session = self.session(&server).await?;
            match session
                .http(self.layout.api_port, method, &path, Some(&auth), body)
                .await
            {
                Ok(response) => return Ok(response),
                Err(_) if attempt == 0 => self.forget_session(id).await,
                Err(e) => return Err(e.into()),
            }
        }
        unreachable!("the loop returns on its second attempt")
    }

    async fn telemt_json(&self, id: &str, path: &str) -> Result<Value, String> {
        let res = self
            .telemt(id, "GET", path, None)
            .await
            .map_err(|e| e.to_string())?;
        let body: Value = serde_json::from_slice(&res.body).map_err(|e| e.to_string())?;
        if res.status == 200 && body["ok"] == true {
            Ok(body["data"].clone())
        } else {
            Err(body["error"]["message"].as_str().map_or_else(
                || format!("the proxy answered {}", res.status),
                str::to_string,
            ))
        }
    }

    pub async fn status(&self, id: &str) -> Result<Status, ManageError> {
        let server = self.server(id)?;
        let session = self.session(&server).await?;
        let remote = Remote::managed(&session, &self.layout, server.is_root);
        let service = remote.is_active().await?;
        let (info, summary) = (
            self.telemt_json(id, "v1/system/info").await,
            self.telemt_json(id, "v1/stats/summary").await,
        );
        let api_error = info.as_ref().err().cloned();
        Ok(Status {
            service,
            info: info.ok(),
            summary: summary.ok(),
            api_error,
        })
    }

    pub async fn service(&self, id: &str, action: &str) -> Result<(), ManageError> {
        if !matches!(action, "start" | "stop" | "restart") {
            return Err(ManageError::Forbidden);
        }
        let server = self.server(id)?;
        let session = self.session(&server).await?;
        Remote::managed(&session, &self.layout, server.is_root)
            .ctl(&format!("{action} the proxy"), &[action])
            .await?;
        Ok(())
    }

    pub async fn logs(&self, id: &str, lines: u32) -> Result<String, ManageError> {
        let server = self.server(id)?;
        let session = self.session(&server).await?;
        let out = Remote::managed(&session, &self.layout, server.is_root)
            .ctl("Reading logs", &["logs", &lines.min(2000).to_string()])
            .await?;
        Ok(out.stdout)
    }

    /// Replaces the binary with the configured release and restarts telemt.
    pub async fn update(&self, id: &str) -> Result<String, ManageError> {
        let server = self.server(id)?;
        let (source, target, arch) = (
            self.source.clone(),
            self.target.clone(),
            server.arch.clone(),
        );
        let fetched = tokio::task::spawn_blocking(move || source.fetch(&target, &arch))
            .await
            .map_err(|e| ManageError::Other(e.to_string()))??;
        let session = self.session(&server).await?;
        let remote = Remote::managed(&session, &self.layout, server.is_root);
        let tmp = remote
            .upload("Uploading the proxy", &fetched.binary)
            .await?;
        let result = remote.ctl("Updating the proxy", &["update", &tmp]).await;
        remote.remove_temp(&[tmp]).await;
        result?;
        Ok(fetched.tag)
    }

    /// Forgets a server. With `uninstall`, first removes telemt from it; the
    /// router key is always removed from authorized_keys when reachable.
    /// Returns a warning when the server could not be cleaned up.
    pub async fn remove(&self, id: &str, uninstall: bool) -> Result<Option<String>, ManageError> {
        let server = self.server(id)?;
        let cleanup = async {
            let session = self.session(&server).await?;
            let remote = Remote::managed(&session, &self.layout, server.is_root);
            if uninstall {
                remote.ctl("Uninstalling the proxy", &["uninstall"]).await?;
            }
            remote.revoke_key(&authorized_line(&self.key)).await?;
            Ok::<_, ManageError>(())
        }
        .await;
        match cleanup {
            // Uninstall was asked for and failed: keep the server so it can be retried.
            Err(e) if uninstall => Err(e),
            result => {
                self.forget_session(id).await;
                self.store
                    .remove(id)
                    .map_err(|e| ManageError::Other(e.to_string()))?;
                Ok(result.err().map(|e| {
                    format!("Removed from the list, but the server could not be cleaned up: {e}")
                }))
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn allows_only_user_and_read_routes() {
        for (m, p) in [
            ("GET", "v1/users"),
            ("POST", "v1/users"),
            ("PATCH", "v1/users/alice"),
            ("DELETE", "v1/users/alice"),
            ("POST", "v1/users/alice/rotate-secret"),
            ("POST", "v1/users/a.b_c-d/reset-quota"),
            ("GET", "v1/system/info"),
            ("PATCH", "v1/config"),
        ] {
            assert!(telemt_route_allowed(m, p), "{m} {p}");
        }
        for (m, p) in [
            ("POST", "v1/system/reload"),
            ("DELETE", "v1/users"),
            ("POST", "v1/users/alice/rename"),
            ("GET", "v1/users/../config"),
            ("GET", "v1/users/a%20b"),
            ("GET", "v1/users?x=1"),
            ("POST", "v1/runtime/web/lifecycle/pause"),
            ("PUT", "v1/config"),
        ] {
            assert!(!telemt_route_allowed(m, p), "{m} {p}");
        }
    }
}
