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
    #[error("invalid {0} {1:?}: use letters, digits, '.', '_' or '-'")]
    InvalidName(&'static str, String),
    #[error("invalid ASSET_PATTERN {0:?}: must be non-empty and contain no '/'")]
    InvalidAssetPattern(String),
    #[error("invalid EXEC_ARGS: must be a single line")]
    InvalidExecArgs,
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

/// What gets deployed to the remote server.
#[derive(Debug, Clone, PartialEq)]
pub struct Target {
    /// GitHub repository, `owner/name`.
    pub repo: String,
    /// Release tag to install; empty means the latest release.
    pub release_tag: String,
    /// Glob over release asset names; `{arch}` expands to the server's arch.
    pub asset_pattern: String,
    /// Executable to take from an archive asset, installed as /usr/local/bin/<name>.
    pub binary_name: String,
    /// systemd unit name, without `.service`.
    pub service_name: String,
    /// Arguments for ExecStart; `{port}` and `{secret}` come from the form.
    pub exec_args: String,
}

impl Default for Target {
    fn default() -> Self {
        Self {
            repo: "9seconds/mtg".into(),
            release_tag: String::new(),
            asset_pattern: "mtg-*-linux-{arch}.tar.gz".into(),
            binary_name: "mtg".into(),
            service_name: "mtg".into(),
            exec_args: "simple-run 0.0.0.0:{port} {secret}".into(),
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

    let port = get("PORT", "8080");
    let target = Target {
        repo: validate_repo(get("GITHUB_REPO", &defaults.repo))?,
        release_tag: get("RELEASE_TAG", ""),
        asset_pattern: validate_pattern(get("ASSET_PATTERN", &defaults.asset_pattern))?,
        binary_name: validate_name("BINARY_NAME", get("BINARY_NAME", &defaults.binary_name))?,
        service_name: validate_name("SERVICE_NAME", get("SERVICE_NAME", &defaults.service_name))?,
        exec_args: validate_exec_args(get("EXEC_ARGS", &defaults.exec_args))?,
    };
    if !target.release_tag.is_empty() && !is_safe_segment(&target.release_tag) {
        return Err(ConfigError::InvalidName("RELEASE_TAG", target.release_tag));
    }

    Ok(Config {
        host: get("HOST", "0.0.0.0"),
        port: port.parse().map_err(|_| ConfigError::InvalidPort(port))?,
        base_path: normalize_base_path(&get("BASE_PATH", ""))?,
        data_dir: PathBuf::from(get("DATA_DIR", "/data")),
        target,
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

fn validate_repo(repo: String) -> Result<String, ConfigError> {
    match repo.split_once('/') {
        Some((owner, name)) if is_safe_segment(owner) && is_safe_segment(name) => Ok(repo),
        _ => Err(ConfigError::InvalidRepo(repo)),
    }
}

fn validate_name(key: &'static str, name: String) -> Result<String, ConfigError> {
    let ok = is_safe_segment(&name) && !name.starts_with(['.', '-']);
    if ok {
        Ok(name)
    } else {
        Err(ConfigError::InvalidName(key, name))
    }
}

fn validate_pattern(pattern: String) -> Result<String, ConfigError> {
    if pattern.contains('/') || pattern.chars().any(char::is_whitespace) {
        return Err(ConfigError::InvalidAssetPattern(pattern));
    }
    Ok(pattern)
}

fn validate_exec_args(args: String) -> Result<String, ConfigError> {
    if args.contains(['\n', '\r']) {
        return Err(ConfigError::InvalidExecArgs);
    }
    Ok(args)
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
    fn defaults_deploy_mtg() {
        let cfg = load(&[]).unwrap();
        assert_eq!(cfg.port, 8080);
        assert_eq!(cfg.base_path, "");
        assert_eq!(cfg.target, Target::default());
    }

    #[test]
    fn blank_values_fall_back_to_defaults() {
        let cfg = load(&[("GITHUB_REPO", "  "), ("EXEC_ARGS", "")]).unwrap();
        assert_eq!(cfg.target.repo, "9seconds/mtg");
        assert_eq!(cfg.target.exec_args, Target::default().exec_args);
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
    fn rejects_unsafe_target_values() {
        assert!(load(&[("GITHUB_REPO", "mtg")]).is_err());
        assert!(load(&[("GITHUB_REPO", "a/b/c")]).is_err());
        assert!(load(&[("SERVICE_NAME", "mtg;reboot")]).is_err());
        assert!(load(&[("BINARY_NAME", "../mtg")]).is_err());
        assert!(load(&[("BINARY_NAME", "-rf")]).is_err());
        assert!(load(&[("ASSET_PATTERN", "a/b")]).is_err());
        assert!(load(&[("EXEC_ARGS", "run\nExecStartPre=/bin/sh")]).is_err());
        assert!(load(&[("RELEASE_TAG", "v1 2")]).is_err());
        assert!(load(&[("PORT", "http")]).is_err());
    }
}
