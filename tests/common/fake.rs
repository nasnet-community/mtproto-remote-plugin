//! A fake server for end-to-end tests: the in-process SSH server runs commands
//! with fake `sudo`, `systemctl`, `journalctl`, `useradd`, `userdel`, `visudo`,
//! `install`, `id` and `uname` first on PATH, telemt's files live under a temp
//! root (the [`Layout`] root), HOME is a temp dir, and a fake telemt API
//! answers on a loopback port.

use std::fs;
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use axum::Json;
use axum::extract::{Path as UrlPath, State};
use axum::http::{HeaderMap, StatusCode};
use axum::response::{IntoResponse, Response};
use axum::routing::{delete, get};
use mtproto_remote::config::Target;
use mtproto_remote::deploy::{Deployer, Fetched, Request, Source};
use mtproto_remote::hostkeys::HostKeys;
use mtproto_remote::keys;
use mtproto_remote::manage::Manager;
use mtproto_remote::release::ReleaseError;
use mtproto_remote::servers::ServerStore;
use mtproto_remote::telemt::Layout;
use russh::keys::PrivateKey;
use serde_json::{Value, json};

use super::{HOST_FINGERPRINT, Login, PASSWORD, USER};

pub const BINARY: &[u8] = b"\x7fELF fake telemt";

/// Serves a fixed binary; `tag` changes what an update installs.
pub struct FakeSource {
    pub tag: Mutex<String>,
}

impl Source for FakeSource {
    fn fetch(&self, _: &Target, arch: &str) -> Result<Fetched, ReleaseError> {
        let tag = self.tag.lock().unwrap().clone();
        Ok(Fetched {
            binary: format!("{}{tag}", String::from_utf8_lossy(BINARY)).into_bytes(),
            asset: format!("telemt-{arch}-linux-musl.tar.gz"),
            tag,
        })
    }
}

#[derive(Clone, Copy)]
pub struct Options {
    pub uid: u32,
    pub sudo_allowed: bool,
    /// Whether the fake telemt API listens at all.
    pub api_up: bool,
}

impl Default for Options {
    fn default() -> Self {
        Self {
            uid: 1000,
            sudo_allowed: true,
            api_up: true,
        }
    }
}

pub struct Fake {
    pub root: PathBuf,
    pub layout: Layout,
    pub ssh_port: u16,
    pub key: Arc<PrivateKey>,
    pub store: Arc<ServerStore>,
    pub source: Arc<FakeSource>,
}

impl Drop for Fake {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.root);
    }
}

fn script(dir: &Path, name: &str, body: &str) {
    let path = dir.join(name);
    fs::write(&path, format!("#!/bin/sh\n{body}\n")).unwrap();
    fs::set_permissions(&path, fs::Permissions::from_mode(0o755)).unwrap();
}

fn write_fakes(bin: &Path, opts: Options) {
    let sudo = if opts.sudo_allowed {
        format!(
            r#"if [ "$1" = -n ]; then
  shift
  grep -qx "{USER} ALL=(root) NOPASSWD: $1" "$ROOT/etc/sudoers.d/nasnet-telemt" 2>/dev/null || {{ echo "sudo: a password is required" >&2; exit 1; }}
  exec "$@"
fi
[ "$1 $2 $3" = "-S -p " ] || {{ echo "fake sudo: bad args $*" >&2; exit 2; }}
shift 3
IFS= read -r pw
[ "$pw" = "{PASSWORD}" ] || {{ echo "Sorry, try again." >&2; exit 1; }}
exec "$@""#
        )
    } else {
        format!(r#"echo "{USER} is not in the sudoers file." >&2; exit 1"#)
    };
    script(bin, "sudo", &sudo);
    script(
        bin,
        "id",
        &format!(
            r#"if [ "$1" = -u ] && [ -n "$2" ]; then grep -qx "$2" "$ROOT/users" 2>/dev/null; exit; fi
echo {}"#,
            opts.uid
        ),
    );
    script(bin, "uname", "echo x86_64");
    script(
        bin,
        "useradd",
        r#"for last; do :; done; echo "$last" >> "$ROOT/users""#,
    );
    script(
        bin,
        "userdel",
        r#"grep -vx "$1" "$ROOT/users" > "$ROOT/users.tmp"; mv "$ROOT/users.tmp" "$ROOT/users""#,
    );
    script(bin, "visudo", r#"[ "$1" = -cf ] && grep -q NOPASSWD "$2""#);
    // Ownership needs root; drop -o/-g and let the real install do the rest.
    script(
        bin,
        "install",
        r#"args=""
while [ $# -gt 0 ]; do
  case "$1" in -o|-g) shift 2 ;; *) args="$args '$1'"; shift ;; esac
done
eval exec /usr/bin/install $args"#,
    );
    script(
        bin,
        "systemctl",
        r#"echo "$*" >> "$ROOT/systemctl.log"
case "$1" in
  is-active) s=$(cat "$ROOT/state" 2>/dev/null || echo inactive); echo "$s"; [ "$s" = active ] ;;
  start|restart) echo active > "$ROOT/state" ;;
  stop) echo inactive > "$ROOT/state" ;;
  disable) [ "$2" = --now ] && echo inactive > "$ROOT/state"; exit 0 ;;
  *) exit 0 ;;
esac"#,
    );
    script(bin, "journalctl", r#"echo "fake log: $*""#);
}

#[derive(Clone)]
struct Api {
    conf: PathBuf,
    users: Arc<Mutex<Vec<String>>>,
}

fn envelope(status: StatusCode, data: Value) -> Response {
    let ok = status.is_success();
    let body = if ok {
        json!({ "ok": true, "data": data, "revision": "r" })
    } else {
        json!({ "ok": false, "error": { "code": "x", "message": data } })
    };
    (status, Json(body)).into_response()
}

impl Api {
    /// Checks the bearer token against the one in the installed config.
    fn authorized(&self, headers: &HeaderMap) -> bool {
        let conf = fs::read_to_string(&self.conf).unwrap_or_default();
        let token = conf
            .lines()
            .find_map(|l| l.strip_prefix("auth_header = \"Bearer "))
            .map(|t| t.trim_end_matches('"'));
        let sent = headers.get("authorization").and_then(|v| v.to_str().ok());
        token.is_some() && sent.and_then(|s| s.strip_prefix("Bearer ")) == token
    }

    fn user(name: &str) -> Value {
        json!({ "username": name, "enabled": true, "links": { "tls": [format!("tg://proxy?secret={name}")] } })
    }
}

async fn list_users(State(api): State<Api>, headers: HeaderMap) -> Response {
    if !api.authorized(&headers) {
        return envelope(StatusCode::UNAUTHORIZED, json!("unauthorized"));
    }
    let users = api
        .users
        .lock()
        .unwrap()
        .iter()
        .map(|n| Api::user(n))
        .collect();
    envelope(StatusCode::OK, Value::Array(users))
}

async fn create_user(
    State(api): State<Api>,
    headers: HeaderMap,
    Json(body): Json<Value>,
) -> Response {
    if !api.authorized(&headers) {
        return envelope(StatusCode::UNAUTHORIZED, json!("unauthorized"));
    }
    let name = body["username"].as_str().unwrap_or_default().to_string();
    let mut users = api.users.lock().unwrap();
    if users.contains(&name) {
        return envelope(StatusCode::CONFLICT, json!("User already exists"));
    }
    users.push(name.clone());
    envelope(
        StatusCode::CREATED,
        json!({ "user": Api::user(&name), "secret": "00" }),
    )
}

async fn delete_user(
    State(api): State<Api>,
    headers: HeaderMap,
    UrlPath(name): UrlPath<String>,
) -> Response {
    if !api.authorized(&headers) {
        return envelope(StatusCode::UNAUTHORIZED, json!("unauthorized"));
    }
    api.users.lock().unwrap().retain(|n| n != &name);
    envelope(
        StatusCode::ACCEPTED,
        json!({ "username": name, "in_runtime": true }),
    )
}

async fn system_info(State(api): State<Api>, headers: HeaderMap) -> Response {
    if !api.authorized(&headers) {
        return envelope(StatusCode::UNAUTHORIZED, json!("unauthorized"));
    }
    envelope(
        StatusCode::OK,
        json!({ "version": "3.5.9", "uptime_seconds": 5.0 }),
    )
}

async fn start_api(conf: PathBuf) -> u16 {
    let api = Api {
        conf,
        users: Arc::new(Mutex::new(vec!["first".into()])),
    };
    let app = axum::Router::new()
        .route("/v1/users", get(list_users).post(create_user))
        .route("/v1/users/{name}", delete(delete_user))
        .route("/v1/system/info", get(system_info))
        .route("/v1/stats/summary", get(system_info))
        .with_state(api);
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let port = listener.local_addr().unwrap().port();
    tokio::spawn(async move { axum::serve(listener, app).await.unwrap() });
    port
}

fn free_port() -> u16 {
    std::net::TcpListener::bind("127.0.0.1:0")
        .unwrap()
        .local_addr()
        .unwrap()
        .port()
}

impl Fake {
    pub async fn new(name: &str, opts: Options) -> Fake {
        let root = std::env::temp_dir().join(format!("mtr-{name}-{}", std::process::id()));
        let _ = fs::remove_dir_all(&root);
        let bin = root.join("fakebin");
        fs::create_dir_all(&bin).unwrap();
        fs::create_dir_all(root.join("home")).unwrap();
        write_fakes(&bin, opts);

        let root_str = root.to_str().unwrap().to_string();
        let conf = PathBuf::from(format!("{root_str}/etc/telemt/telemt.toml"));
        let api_port = if opts.api_up {
            start_api(conf).await
        } else {
            free_port()
        };
        let layout = Layout {
            root: root_str.clone(),
            api_port,
        };
        let prelude = format!(
            "export ROOT='{root_str}' HOME='{root_str}/home' PATH='{root_str}/fakebin':\"$PATH\"; "
        );
        let ssh_port = super::start_with(
            Login::Password,
            &prelude,
            Some(root.join("home/.ssh/authorized_keys")),
        )
        .await;

        Fake {
            key: Arc::new(keys::load_or_create(&root.join("data/id_ed25519")).unwrap()),
            store: Arc::new(ServerStore::open(root.join("data/servers.json")).unwrap()),
            source: Arc::new(FakeSource {
                tag: Mutex::new("3.5.9".into()),
            }),
            root,
            layout,
            ssh_port,
        }
    }

    pub fn deployer(&self) -> Deployer {
        Deployer {
            target: Target::default(),
            source: self.source.clone(),
            host_keys: HostKeys::new(self.root.join("data/known_hosts.json")),
            layout: self.layout.clone(),
            key: self.key.clone(),
            store: self.store.clone(),
            api_wait: Duration::from_secs(1),
        }
    }

    pub fn manager(&self) -> Manager {
        Manager::new(
            self.store.clone(),
            self.key.clone(),
            self.layout.clone(),
            Target::default(),
            self.source.clone(),
        )
    }

    pub fn request(&self) -> Request {
        Request {
            host: "127.0.0.1".into(),
            ssh_port: self.ssh_port,
            username: USER.into(),
            password: PASSWORD.into(),
            fingerprint: HOST_FINGERPRINT.into(),
            name: "Test box".into(),
            proxy_port: 443,
            tls_domain: "www.google.com".into(),
            first_user: "first".into(),
        }
    }

    /// Reads a file by its path on the "server" (under the root).
    pub fn read(&self, path: &str) -> String {
        fs::read_to_string(format!("{}{path}", self.layout.root)).unwrap_or_default()
    }

    pub fn exists(&self, path: &str) -> bool {
        Path::new(&format!("{}{path}", self.layout.root)).exists()
    }

    pub fn authorized_keys(&self) -> String {
        fs::read_to_string(self.root.join("home/.ssh/authorized_keys")).unwrap_or_default()
    }
}
