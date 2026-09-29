//! The servers this router manages, kept as JSON on the data mount.

use std::fs::{self, OpenOptions};
use std::io::{self, Write};
use std::os::unix::fs::OpenOptionsExt;
use std::path::PathBuf;
use std::sync::Mutex;

use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Server {
    pub id: String,
    pub name: String,
    pub host: String,
    pub ssh_port: u16,
    pub username: String,
    /// Host key fingerprint confirmed at install.
    pub fingerprint: String,
    pub is_root: bool,
    /// Bearer token for telemt's control API. Never sent to the page.
    pub api_token: String,
    pub proxy_port: u16,
    pub tls_domain: String,
    pub arch: String,
    pub added_at: u64,
}

/// What the page sees of a server.
#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct ServerView {
    pub id: String,
    pub name: String,
    pub host: String,
    pub ssh_port: u16,
    pub username: String,
    pub fingerprint: String,
    pub proxy_port: u16,
    pub tls_domain: String,
    pub arch: String,
    pub added_at: u64,
}

impl From<&Server> for ServerView {
    fn from(s: &Server) -> Self {
        Self {
            id: s.id.clone(),
            name: s.name.clone(),
            host: s.host.clone(),
            ssh_port: s.ssh_port,
            username: s.username.clone(),
            fingerprint: s.fingerprint.clone(),
            proxy_port: s.proxy_port,
            tls_domain: s.tls_domain.clone(),
            arch: s.arch.clone(),
            added_at: s.added_at,
        }
    }
}

pub struct ServerStore {
    path: PathBuf,
    servers: Mutex<Vec<Server>>,
}

impl ServerStore {
    pub fn open(path: impl Into<PathBuf>) -> io::Result<Self> {
        let path = path.into();
        let servers = match fs::read(&path) {
            Ok(bytes) => serde_json::from_slice(&bytes).map_err(io::Error::other)?,
            Err(e) if e.kind() == io::ErrorKind::NotFound => Vec::new(),
            Err(e) => return Err(e),
        };
        Ok(Self {
            path,
            servers: Mutex::new(servers),
        })
    }

    pub fn list(&self) -> Vec<Server> {
        self.servers.lock().expect("store lock").clone()
    }

    pub fn get(&self, id: &str) -> Option<Server> {
        self.list().into_iter().find(|s| s.id == id)
    }

    /// Adds `server`, or replaces the entry with the same id, or for the same
    /// host, port and user (keeping that entry's id). Returns the stored server.
    pub fn upsert(&self, mut server: Server) -> io::Result<Server> {
        let mut servers = self.servers.lock().expect("store lock");
        let same = |s: &Server| {
            s.id == server.id
                || (s.host.eq_ignore_ascii_case(&server.host)
                    && s.ssh_port == server.ssh_port
                    && s.username == server.username)
        };
        match servers.iter_mut().find(|s| same(s)) {
            Some(existing) => {
                server.id = existing.id.clone();
                *existing = server.clone();
            }
            None => servers.push(server.clone()),
        }
        self.persist(&servers)?;
        Ok(server)
    }

    pub fn rename(&self, id: &str, name: &str) -> io::Result<Option<Server>> {
        let mut servers = self.servers.lock().expect("store lock");
        let Some(server) = servers.iter_mut().find(|s| s.id == id) else {
            return Ok(None);
        };
        server.name = name.to_string();
        let updated = server.clone();
        self.persist(&servers)?;
        Ok(Some(updated))
    }

    pub fn remove(&self, id: &str) -> io::Result<bool> {
        let mut servers = self.servers.lock().expect("store lock");
        let before = servers.len();
        servers.retain(|s| s.id != id);
        if servers.len() == before {
            return Ok(false);
        }
        self.persist(&servers)?;
        Ok(true)
    }

    fn persist(&self, servers: &[Server]) -> io::Result<()> {
        if let Some(dir) = self.path.parent() {
            fs::create_dir_all(dir)?;
        }
        let tmp = self.path.with_extension("tmp");
        let mut file = OpenOptions::new()
            .write(true)
            .create(true)
            .truncate(true)
            .mode(0o600)
            .open(&tmp)?;
        file.write_all(&serde_json::to_vec_pretty(servers)?)?;
        file.sync_all()?;
        fs::rename(tmp, &self.path)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn server(id: &str, host: &str) -> Server {
        Server {
            id: id.into(),
            name: host.into(),
            host: host.into(),
            ssh_port: 22,
            username: "root".into(),
            fingerprint: "SHA256:x".into(),
            is_root: true,
            api_token: "secret-token".into(),
            proxy_port: 443,
            tls_domain: "www.google.com".into(),
            arch: "x86_64".into(),
            added_at: 1,
        }
    }

    #[test]
    fn persists_updates_and_removes_servers() {
        let dir = std::env::temp_dir().join(format!("servers-test-{}", std::process::id()));
        let path = dir.join("servers.json");
        let store = ServerStore::open(&path).unwrap();
        assert!(store.list().is_empty());

        store.upsert(server("a", "one.example")).unwrap();
        store.upsert(server("b", "two.example")).unwrap();
        // Reinstalling on the same server keeps its id.
        let again = store.upsert(server("c", "ONE.example")).unwrap();
        assert_eq!(again.id, "a");
        assert_eq!(store.list().len(), 2);

        // Saving a known id with new details updates it in place.
        let mut moved = store.get("b").unwrap();
        moved.ssh_port = 2222;
        store.upsert(moved).unwrap();
        assert_eq!(store.list().len(), 2);
        assert_eq!(store.get("b").unwrap().ssh_port, 2222);

        store.rename("b", "Frankfurt").unwrap();
        let reopened = ServerStore::open(&path).unwrap();
        assert_eq!(reopened.get("b").unwrap().name, "Frankfurt");

        assert!(reopened.remove("a").unwrap());
        assert!(!reopened.remove("a").unwrap());
        assert_eq!(ServerStore::open(&path).unwrap().list().len(), 1);

        use std::os::unix::fs::PermissionsExt;
        assert_eq!(
            fs::metadata(&path).unwrap().permissions().mode() & 0o777,
            0o600
        );
        fs::remove_dir_all(dir).unwrap();
    }

    #[test]
    fn the_page_view_leaves_out_the_api_token() {
        let json = serde_json::to_string(&ServerView::from(&server("a", "h"))).unwrap();
        assert!(!json.contains("secret-token"));
        assert!(json.contains("\"sshPort\":22"));
    }
}
