//! The router's own SSH key, which managed servers trust after install.

use std::fs::{self, File, OpenOptions};
use std::io::{self, Read, Write};
use std::os::unix::fs::OpenOptionsExt;
use std::path::Path;

use russh::keys::PrivateKey;
use russh::keys::ssh_key::LineEnding;
use russh::keys::ssh_key::private::Ed25519Keypair;

/// Comment on the public key, so it is recognisable in authorized_keys.
pub const KEY_COMMENT: &str = "nasnet-mtproto-remote";

/// Reads `n` random bytes from the kernel.
pub fn random_bytes<const N: usize>() -> io::Result<[u8; N]> {
    let mut buf = [0u8; N];
    File::open("/dev/urandom")?.read_exact(&mut buf)?;
    Ok(buf)
}

pub fn random_hex<const N: usize>() -> io::Result<String> {
    Ok(random_bytes::<N>()?
        .iter()
        .map(|b| format!("{b:02x}"))
        .collect())
}

/// Loads the key at `path`, or creates a new ed25519 key there (mode 0600).
pub fn load_or_create(path: &Path) -> io::Result<PrivateKey> {
    if path.exists() {
        let pem = fs::read_to_string(path)?;
        return PrivateKey::from_openssh(pem).map_err(io::Error::other);
    }
    let mut key = PrivateKey::from(Ed25519Keypair::from_seed(&random_bytes::<32>()?));
    key.set_comment(KEY_COMMENT);
    let pem = key.to_openssh(LineEnding::LF).map_err(io::Error::other)?;

    if let Some(dir) = path.parent() {
        fs::create_dir_all(dir)?;
    }
    let tmp = path.with_extension("tmp");
    let mut file = OpenOptions::new()
        .write(true)
        .create(true)
        .truncate(true)
        .mode(0o600)
        .open(&tmp)?;
    file.write_all(pem.as_bytes())?;
    file.sync_all()?;
    fs::rename(tmp, path)?;
    Ok(key)
}

/// The key as one authorized_keys line: `ssh-ed25519 AAAA… nasnet-mtproto-remote`.
pub fn authorized_line(key: &PrivateKey) -> String {
    key.public_key()
        .to_openssh()
        .expect("an ed25519 public key always encodes")
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::PermissionsExt;

    #[test]
    fn creates_once_then_loads_the_same_key() {
        let dir = std::env::temp_dir().join(format!("keys-test-{}", std::process::id()));
        let path = dir.join("id_ed25519");

        let created = load_or_create(&path).unwrap();
        let mode = fs::metadata(&path).unwrap().permissions().mode() & 0o777;
        assert_eq!(mode, 0o600);
        let loaded = load_or_create(&path).unwrap();
        assert_eq!(authorized_line(&created), authorized_line(&loaded));

        let line = authorized_line(&loaded);
        assert!(line.starts_with("ssh-ed25519 AAAA"), "{line}");
        assert!(line.ends_with(" nasnet-mtproto-remote"), "{line}");
        fs::remove_dir_all(dir).unwrap();
    }

    #[test]
    fn random_hex_has_the_requested_length() {
        let a = random_hex::<16>().unwrap();
        assert_eq!(a.len(), 32);
        assert!(a.chars().all(|c| c.is_ascii_hexdigit()));
        assert_ne!(a, random_hex::<16>().unwrap());
    }
}
