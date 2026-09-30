use std::env;
use std::path::PathBuf;

use thiserror::Error;

#[derive(Debug, Error, PartialEq)]
pub enum ConfigError {
    #[error("invalid PORT {0:?}")]
    InvalidPort(String),
    #[error("invalid BASE_PATH {0:?}: must be a path like /api/plugin/view")]
    InvalidBasePath(String),
    #[error("invalid GITHUB_REPO {0:?}: must be owner/name")]
    InvalidRepo(String),
    #[error("invalid RELEASE_TAG {0:?}")]
    InvalidTag(String),
}

/// Runtime configuration, read from the environment the plugin manifest sets.
#[derive(Debug, Clone)]
pub struct Config {
    pub host: String,
    pub port: u16,
    pub base_path: String,
    pub data_dir: PathBuf,
    pub target: Target,
}

/// Where the telemt release comes from.
#[derive(Debug, Clone, PartialEq)]
pub struct Target {
    /// GitHub repository, `owner/name`; a fork or mirror of telemt.
    pub repo: String,
    /// Release tag to install; empty means the latest release.
    pub release_tag: String,
}

impl Default for Target {
    fn default() -> Self {
        Self {
            repo: "telemt/telemt".into(),
            release_tag: String::new(),
        }
    }
}

pub fn from_env() -> Result<Config, ConfigError> {
    from_lookup(|key| env::var(key).ok())
}

pub fn from_lookup(get: impl Fn(&str) -> Option<String>) -> Result<Config, ConfigError> {
    // The panel substitutes an empty string for an unset optional setting, so
    // blank values fall back to the defaults too.
    let get = |key: &str, fallback: &str| {
        get(key)
            .map(|v| v.trim().to_string())
            .filter(|v| !v.is_empty())
            .unwrap_or_else(|| fallback.to_string())
    };
    let defaults = Target::default();

    let repo = get("GITHUB_REPO", &defaults.repo);
    let valid_repo = matches!(repo.split_once('/'),
        Some((owner, name)) if is_safe_segment(owner) && is_safe_segment(name));
    if !valid_repo {
        return Err(ConfigError::InvalidRepo(repo));
    }
    let release_tag = get("RELEASE_TAG", "");
    if !release_tag.is_empty() && !is_safe_segment(&release_tag) {
        return Err(ConfigError::InvalidTag(release_tag));
    }

    let port = get("PORT", "8080");
    Ok(Config {
        host: get("HOST", "0.0.0.0"),
        port: port.parse().map_err(|_| ConfigError::InvalidPort(port))?,
        base_path: normalize_base_path(&get("BASE_PATH", ""))?,
        data_dir: PathBuf::from(get("DATA_DIR", "/data")),
        target: Target { repo, release_tag },
    })
}

pub fn normalize_base_path(raw: &str) -> Result<String, ConfigError> {
    let trimmed = raw.trim().trim_end_matches('/');
    let trimmed = trimmed.strip_prefix('/').unwrap_or(trimmed);
    if trimmed.is_empty() {
        return Ok(String::new());
    }
    if !trimmed.split('/').all(is_safe_segment) {
        return Err(ConfigError::InvalidBasePath(raw.to_string()));
    }
    Ok(format!("/{trimmed}"))
}

fn is_safe_segment(s: &str) -> bool {
    !s.is_empty()
        && s != "."
        && s != ".."
        && s.chars()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, '-' | '_' | '.' | '~'))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashMap;

    fn load(pairs: &[(&str, &str)]) -> Result<Config, ConfigError> {
        let map: HashMap<String, String> = pairs
            .iter()
            .map(|(k, v)| (k.to_string(), v.to_string()))
            .collect();
        from_lookup(|k| map.get(k).cloned())
    }

    #[test]
    fn defaults_install_latest_telemt() {
        let cfg = load(&[]).unwrap();
        assert_eq!(cfg.port, 8080);
        assert_eq!(cfg.base_path, "");
        assert_eq!(cfg.data_dir, PathBuf::from("/data"));
        assert_eq!(cfg.target, Target::default());
    }

    #[test]
    fn blank_values_fall_back_to_defaults() {
        let cfg = load(&[("GITHUB_REPO", "  "), ("RELEASE_TAG", "")]).unwrap();
        assert_eq!(cfg.target, Target::default());
    }

    #[test]
    fn base_path_is_normalized() {
        assert_eq!(normalize_base_path("/").unwrap(), "");
        assert_eq!(
            normalize_base_path("api/plugin/view/mtproto-remote/").unwrap(),
            "/api/plugin/view/mtproto-remote"
        );
        for bad in ["/a//b", "/a b", "/<x>", "/a/../b"] {
            assert!(normalize_base_path(bad).is_err(), "accepted {bad:?}");
        }
    }

    #[test]
    fn rejects_unsafe_values() {
        assert!(load(&[("GITHUB_REPO", "telemt")]).is_err());
        assert!(load(&[("GITHUB_REPO", "a/b/c")]).is_err());
        assert!(load(&[("RELEASE_TAG", "3.5.9 x")]).is_err());
        assert!(load(&[("RELEASE_TAG", "..")]).is_err());
        assert!(load(&[("PORT", "http")]).is_err());
        let pinned = load(&[("RELEASE_TAG", "3.5.9")]).unwrap();
        assert_eq!(pinned.target.release_tag, "3.5.9");
    }
}
