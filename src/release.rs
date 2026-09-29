//! Finding, downloading and unpacking the release asset for a server.

use std::io::Read;
use std::time::Duration;

use flate2::read::GzDecoder;
use serde::Deserialize;
use thiserror::Error;

use crate::config::Target;

/// Largest asset the app will download or unpack.
const MAX_ASSET_BYTES: u64 = 128 * 1024 * 1024;

#[derive(Debug, Error)]
pub enum ReleaseError {
    #[error("unsupported server architecture {0:?}")]
    UnsupportedArch(String),
    #[error("GitHub request failed: {0}")]
    Http(String),
    #[error("no asset in release {tag} matches {pattern:?}; assets: {assets}")]
    NoMatchingAsset {
        tag: String,
        pattern: String,
        assets: String,
    },
    #[error("release {tag} has several assets matching {pattern:?}: {assets}")]
    AmbiguousAsset {
        tag: String,
        pattern: String,
        assets: String,
    },
    #[error("archive {asset} has no file named {binary:?}")]
    BinaryNotInArchive { asset: String, binary: String },
    #[error("could not unpack {asset}: {reason}")]
    Unpack { asset: String, reason: String },
}

#[derive(Debug, Clone, Deserialize)]
pub struct Release {
    pub tag_name: String,
    pub assets: Vec<Asset>,
}

#[derive(Debug, Clone, Deserialize)]
pub struct Asset {
    pub name: String,
    pub size: u64,
    pub browser_download_url: String,
}

/// Maps `uname -m` output to the architecture names release assets use.
pub fn release_arch(uname_m: &str) -> Result<&'static str, ReleaseError> {
    match uname_m.trim() {
        "x86_64" | "amd64" => Ok("amd64"),
        "aarch64" | "arm64" | "armv8l" => Ok("arm64"),
        "armv7l" | "armv7" => Ok("armv7"),
        "armv6l" => Ok("armv6"),
        "i386" | "i686" => Ok("386"),
        "mips" => Ok("mips"),
        "mipsel" | "mipsle" => Ok("mipsle"),
        other => Err(ReleaseError::UnsupportedArch(other.to_string())),
    }
}

/// Glob match where `*` matches any run of characters, including none.
pub fn glob_match(pattern: &str, text: &str) -> bool {
    let (p, t): (Vec<char>, Vec<char>) = (pattern.chars().collect(), text.chars().collect());
    let (mut pi, mut ti) = (0, 0);
    let mut backtrack: Option<(usize, usize)> = None;
    while ti < t.len() {
        if pi < p.len() && p[pi] == '*' {
            backtrack = Some((pi, ti));
            pi += 1;
        } else if pi < p.len() && p[pi] == t[ti] {
            pi += 1;
            ti += 1;
        } else if let Some((star, matched)) = backtrack {
            pi = star + 1;
            ti = matched + 1;
            backtrack = Some((star, matched + 1));
        } else {
            return false;
        }
    }
    p[pi..].iter().all(|&c| c == '*')
}

/// Picks the single asset whose name matches the pattern for `arch`.
pub fn select_asset<'a>(
    release: &'a Release,
    pattern: &str,
    arch: &str,
) -> Result<&'a Asset, ReleaseError> {
    let pattern = pattern.replace("{arch}", arch);
    let names = |assets: &[&Asset]| {
        assets
            .iter()
            .map(|a| a.name.as_str())
            .collect::<Vec<_>>()
            .join(", ")
    };
    let matches: Vec<&Asset> = release
        .assets
        .iter()
        .filter(|a| glob_match(&pattern, &a.name))
        .collect();
    match matches.as_slice() {
        [one] => Ok(one),
        [] => Err(ReleaseError::NoMatchingAsset {
            tag: release.tag_name.clone(),
            pattern,
            assets: names(&release.assets.iter().collect::<Vec<_>>()),
        }),
        many => Err(ReleaseError::AmbiguousAsset {
            tag: release.tag_name.clone(),
            pattern,
            assets: names(many),
        }),
    }
}

/// Returns the executable inside a downloaded asset: the file named
/// `binary_name` from a `.tar.gz`/`.tgz`, or the asset itself otherwise.
pub fn extract_binary(
    asset_name: &str,
    bytes: &[u8],
    binary_name: &str,
) -> Result<Vec<u8>, ReleaseError> {
    let is_tarball = asset_name.ends_with(".tar.gz") || asset_name.ends_with(".tgz");
    if !is_tarball {
        return Ok(bytes.to_vec());
    }
    let unpack = |reason: std::io::Error| ReleaseError::Unpack {
        asset: asset_name.to_string(),
        reason: reason.to_string(),
    };

    let mut archive = tar::Archive::new(GzDecoder::new(bytes));
    for entry in archive.entries().map_err(unpack)? {
        let entry = entry.map_err(unpack)?;
        if !entry.header().entry_type().is_file() {
            continue;
        }
        let path = entry.path().map_err(unpack)?;
        if path.file_name().and_then(|n| n.to_str()) != Some(binary_name) {
            continue;
        }
        let mut out = Vec::new();
        entry
            .take(MAX_ASSET_BYTES + 1)
            .read_to_end(&mut out)
            .map_err(unpack)?;
        if out.len() as u64 > MAX_ASSET_BYTES {
            return Err(ReleaseError::Unpack {
                asset: asset_name.to_string(),
                reason: format!("{binary_name} is larger than {MAX_ASSET_BYTES} bytes"),
            });
        }
        return Ok(out);
    }
    Err(ReleaseError::BinaryNotInArchive {
        asset: asset_name.to_string(),
        binary: binary_name.to_string(),
    })
}

/// Blocking GitHub client; run it on a blocking thread.
pub struct GitHub {
    agent: ureq::Agent,
    api_base: String,
}

impl Default for GitHub {
    fn default() -> Self {
        Self::new("https://api.github.com")
    }
}

impl GitHub {
    pub fn new(api_base: &str) -> Self {
        let config = ureq::Agent::config_builder()
            .timeout_connect(Some(Duration::from_secs(15)))
            .timeout_global(Some(Duration::from_secs(300)))
            .user_agent(concat!("mtproto-remote/", env!("CARGO_PKG_VERSION")))
            .build();
        Self {
            agent: config.into(),
            api_base: api_base.trim_end_matches('/').to_string(),
        }
    }

    pub fn release(&self, target: &Target) -> Result<Release, ReleaseError> {
        let which = if target.release_tag.is_empty() {
            "latest".to_string()
        } else {
            format!("tags/{}", target.release_tag)
        };
        let url = format!("{}/repos/{}/releases/{which}", self.api_base, target.repo);
        self.agent
            .get(&url)
            .header("Accept", "application/vnd.github+json")
            .call()
            .map_err(|e| ReleaseError::Http(format!("{url}: {e}")))?
            .body_mut()
            .read_json()
            .map_err(|e| ReleaseError::Http(format!("{url}: {e}")))
    }

    pub fn download(&self, asset: &Asset) -> Result<Vec<u8>, ReleaseError> {
        let url = &asset.browser_download_url;
        self.agent
            .get(url)
            .call()
            .map_err(|e| ReleaseError::Http(format!("{url}: {e}")))?
            .body_mut()
            .with_config()
            .limit(MAX_ASSET_BYTES)
            .read_to_vec()
            .map_err(|e| ReleaseError::Http(format!("{url}: {e}")))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use flate2::{Compression, write::GzEncoder};

    fn release(names: &[&str]) -> Release {
        Release {
            tag_name: "v2.2.8".into(),
            assets: names
                .iter()
                .map(|n| Asset {
                    name: n.to_string(),
                    size: 1,
                    browser_download_url: format!("https://example.invalid/{n}"),
                })
                .collect(),
        }
    }

    fn tarball(files: &[(&str, &[u8])]) -> Vec<u8> {
        let mut builder = tar::Builder::new(GzEncoder::new(Vec::new(), Compression::fast()));
        builder
            .append_dir("mtg-2.2.8-linux-amd64", ".")
            .expect("dir entry");
        for (path, data) in files {
            let mut header = tar::Header::new_gnu();
            header.set_size(data.len() as u64);
            header.set_mode(0o755);
            header.set_cksum();
            builder.append_data(&mut header, path, *data).unwrap();
        }
        builder.into_inner().unwrap().finish().unwrap()
    }

    #[test]
    fn maps_uname_to_release_arch() {
        assert_eq!(release_arch("x86_64\n").unwrap(), "amd64");
        assert_eq!(release_arch("aarch64").unwrap(), "arm64");
        assert_eq!(release_arch("armv7l").unwrap(), "armv7");
        assert!(release_arch("riscv64").is_err());
    }

    #[test]
    fn glob_matches_stars_anywhere() {
        assert!(glob_match(
            "mtg-*-linux-amd64.tar.gz",
            "mtg-2.2.8-linux-amd64.tar.gz"
        ));
        assert!(!glob_match(
            "mtg-*-linux-amd64.tar.gz",
            "mtg-2.2.8-linux-amd64-v3.tar.gz"
        ));
        assert!(glob_match("*", ""));
        assert!(glob_match("a*b*c", "a-b-b-c"));
        assert!(!glob_match("a*b", "a-b-c"));
        assert!(glob_match("exact", "exact"));
    }

    #[test]
    fn selects_the_plain_arch_build_from_mtg_assets() {
        let rel = release(&[
            "mtg-2.2.8-checksums.txt",
            "mtg-2.2.8-darwin-amd64.tar.gz",
            "mtg-2.2.8-linux-amd64-v3.tar.gz",
            "mtg-2.2.8-linux-amd64.tar.gz",
            "mtg-2.2.8-linux-arm64-v9.0.tar.gz",
            "mtg-2.2.8-linux-arm64.tar.gz",
        ]);
        let pattern = Target::default().asset_pattern;
        assert_eq!(
            select_asset(&rel, &pattern, "amd64").unwrap().name,
            "mtg-2.2.8-linux-amd64.tar.gz"
        );
        assert_eq!(
            select_asset(&rel, &pattern, "arm64").unwrap().name,
            "mtg-2.2.8-linux-arm64.tar.gz"
        );
        assert!(matches!(
            select_asset(&rel, &pattern, "mips"),
            Err(ReleaseError::NoMatchingAsset { .. })
        ));
        assert!(matches!(
            select_asset(&rel, "mtg-*-linux-{arch}*", "amd64"),
            Err(ReleaseError::AmbiguousAsset { .. })
        ));
    }

    #[test]
    fn extracts_the_named_binary_from_a_tarball() {
        let archive = tarball(&[
            ("mtg-2.2.8-linux-amd64/README.md", b"docs"),
            ("mtg-2.2.8-linux-amd64/mtg", b"\x7fELF-binary"),
        ]);
        let bin = extract_binary("mtg-2.2.8-linux-amd64.tar.gz", &archive, "mtg").unwrap();
        assert_eq!(bin, b"\x7fELF-binary");
        assert!(matches!(
            extract_binary("x.tar.gz", &archive, "other"),
            Err(ReleaseError::BinaryNotInArchive { .. })
        ));
    }

    #[test]
    fn uses_a_plain_asset_as_the_binary() {
        assert_eq!(
            extract_binary("tool-linux-amd64", b"raw", "tool").unwrap(),
            b"raw"
        );
    }

    #[test]
    fn rejects_a_corrupt_tarball() {
        assert!(matches!(
            extract_binary("x.tar.gz", b"not gzip", "mtg"),
            Err(ReleaseError::Unpack { .. })
        ));
    }

    /// Hits the real GitHub API; run with `cargo test -- --ignored`.
    #[test]
    #[ignore]
    fn live_mtg_release_downloads_and_unpacks() {
        let target = Target::default();
        let github = GitHub::default();
        let rel = github.release(&target).unwrap();
        let asset = select_asset(&rel, &target.asset_pattern, "amd64").unwrap();
        let bytes = github.download(asset).unwrap();
        assert_eq!(bytes.len() as u64, asset.size);
        let bin = extract_binary(&asset.name, &bytes, &target.binary_name).unwrap();
        assert_eq!(&bin[..4], b"\x7fELF");
    }
}
