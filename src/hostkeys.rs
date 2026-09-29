//! Host keys the user has confirmed, kept as JSON on the data mount.

use std::collections::BTreeMap;
use std::fs;
use std::io;
use std::path::PathBuf;

use serde::Serialize;

#[derive(Debug, Clone, Copy, PartialEq, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum KeyStatus {
    /// Never seen this server before.
    New,
    /// Same key as last time.
    Known,
    /// The key differs from the confirmed one.
    Changed,
}

pub struct HostKeys {
    path: PathBuf,
}

impl HostKeys {
    pub fn new(path: impl Into<PathBuf>) -> Self {
        Self { path: path.into() }
    }

    fn id(host: &str, port: u16) -> String {
        format!("{}:{port}", host.trim().to_ascii_lowercase())
    }

    fn load(&self) -> BTreeMap<String, String> {
        fs::read(&self.path)
            .ok()
            .and_then(|bytes| serde_json::from_slice(&bytes).ok())
            .unwrap_or_default()
    }

    pub fn status(&self, host: &str, port: u16, fingerprint: &str) -> KeyStatus {
        match self.load().get(&Self::id(host, port)) {
            None => KeyStatus::New,
            Some(saved) if saved == fingerprint => KeyStatus::Known,
            Some(_) => KeyStatus::Changed,
        }
    }

    pub fn save(&self, host: &str, port: u16, fingerprint: &str) -> io::Result<()> {
        let mut keys = self.load();
        keys.insert(Self::id(host, port), fingerprint.to_string());
        if let Some(dir) = self.path.parent() {
            fs::create_dir_all(dir)?;
        }
        let tmp = self.path.with_extension("tmp");
        fs::write(&tmp, serde_json::to_vec_pretty(&keys)?)?;
        fs::rename(tmp, &self.path)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn tracks_new_known_and_changed_keys() {
        let dir = std::env::temp_dir().join(format!("hostkeys-{}", std::process::id()));
        let keys = HostKeys::new(dir.join("known_hosts.json"));

        assert_eq!(keys.status("vps.example", 22, "SHA256:a"), KeyStatus::New);
        keys.save("VPS.example", 22, "SHA256:a").unwrap();
        assert_eq!(keys.status("vps.example", 22, "SHA256:a"), KeyStatus::Known);
        assert_eq!(
            keys.status("vps.example", 22, "SHA256:b"),
            KeyStatus::Changed
        );
        assert_eq!(keys.status("vps.example", 2222, "SHA256:a"), KeyStatus::New);

        fs::remove_dir_all(dir).unwrap();
    }
}
