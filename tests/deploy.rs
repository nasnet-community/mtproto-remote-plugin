//! End-to-end deployments against the in-process SSH server. Commands run in a
//! local shell whose PATH starts with fake `sudo`, `id`, `uname`, `install` and
//! `systemctl`, which write under a temp root instead of the real system.

mod common;

use std::fs;
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use common::{HOST_FINGERPRINT, Login, PASSWORD, USER};
use mtproto_remote::config::Target;
use mtproto_remote::deploy::{Deployer, Fetched, Request, Source, StepState, Tracker};
use mtproto_remote::hostkeys::{HostKeys, KeyStatus};
use mtproto_remote::release::ReleaseError;

const BINARY: &[u8] = b"\x7fELF fake mtg";
const SECRET: &str = "ee0123456789abcdef0123456789abcdef676f6f676c652e636f6d";

struct FakeSource;

impl Source for FakeSource {
    fn fetch(&self, _: &Target, arch: &str) -> Result<Fetched, ReleaseError> {
        Ok(Fetched {
            tag: "v2.2.8".into(),
            asset: format!("mtg-2.2.8-linux-{arch}.tar.gz"),
            binary: BINARY.to_vec(),
        })
    }
}

struct Fake {
    root: PathBuf,
}

impl Drop for Fake {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.root);
    }
}

#[derive(Clone, Copy)]
struct Server {
    uid: u32,
    sudo_allowed: bool,
    service_starts: bool,
}

impl Default for Server {
    fn default() -> Self {
        Self {
            uid: 1000,
            sudo_allowed: true,
            service_starts: true,
        }
    }
}

fn script(dir: &Path, name: &str, body: &str) {
    let path = dir.join(name);
    fs::write(&path, format!("#!/bin/sh\n{body}\n")).unwrap();
    fs::set_permissions(&path, fs::Permissions::from_mode(0o755)).unwrap();
}

impl Fake {
    fn new(name: &str, server: Server) -> Fake {
        let root =
            std::env::temp_dir().join(format!("mtproto-remote-{name}-{}", std::process::id()));
        let _ = fs::remove_dir_all(&root);
        let bin = root.join("fakebin");
        fs::create_dir_all(&bin).unwrap();

        let sudo = if server.sudo_allowed {
            format!(
                r#"[ "$1 $2 $3" = "-S -p " ] || {{ echo "fake sudo: bad args $*" >&2; exit 2; }}
shift 3
IFS= read -r pw
[ "$pw" = "{PASSWORD}" ] || {{ echo "Sorry, try again." >&2; exit 1; }}
exec "$@""#
            )
        } else {
            format!(r#"echo "{USER} is not in the sudoers file." >&2; exit 1"#)
        };
        script(&bin, "sudo", &sudo);
        script(&bin, "id", &format!("echo {}", server.uid));
        script(&bin, "uname", "echo x86_64");
        script(
            &bin,
            "install",
            r#"[ "$1" = -m ] || exit 2
mkdir -p "$ROOT$(dirname "$4")" && cp "$3" "$ROOT$4" && chmod "$2" "$ROOT$4""#,
        );
        let active = if server.service_starts {
            "echo active"
        } else {
            "echo failed; exit 3"
        };
        script(
            &bin,
            "systemctl",
            &format!(
                r#"echo "$*" >> "$ROOT/systemctl.log"
case "$1" in
  is-active) {active} ;;
  status) echo "* $2.service - fake status" ;;
esac
exit 0"#
            ),
        );
        Fake { root }
    }

    fn prelude(&self) -> String {
        format!(
            "export ROOT='{root}' PATH='{root}/fakebin':\"$PATH\"; ",
            root = self.root.display()
        )
    }

    fn read(&self, path: &str) -> Vec<u8> {
        fs::read(self.root.join(path.trim_start_matches('/'))).unwrap_or_default()
    }

    fn deployer(&self) -> Deployer {
        Deployer {
            target: Target::default(),
            source: Arc::new(FakeSource),
            host_keys: HostKeys::new(self.root.join("data/known_hosts.json")),
            settle: Duration::ZERO,
        }
    }
}

fn request(port: u16) -> Request {
    Request {
        host: "127.0.0.1".into(),
        ssh_port: port,
        username: USER.into(),
        password: PASSWORD.into(),
        fingerprint: HOST_FINGERPRINT.into(),
        proxy_port: 443,
        secret: SECRET.into(),
    }
}

async fn run(name: &str, server: Server, edit: impl FnOnce(&mut Request)) -> (Fake, Tracker) {
    let fake = Fake::new(name, server);
    let port = common::start(Login::Password, &fake.prelude()).await;
    let mut req = request(port);
    edit(&mut req);
    let tracker = Tracker::default();
    tracker.start().unwrap();
    fake.deployer().run(req, &tracker).await;
    (fake, tracker)
}

#[tokio::test]
async fn deploys_through_sudo_and_starts_the_service() {
    let (fake, tracker) = run("sudo", Server::default(), |_| {}).await;
    let job = tracker.snapshot();

    assert_eq!(job.error, None, "{job:#?}");
    assert!(!job.running);
    assert!(
        job.steps.iter().all(|s| s.state == StepState::Done),
        "{job:#?}"
    );
    assert_eq!(job.steps[1].detail, "x86_64 (amd64), systemd, sudo");
    assert_eq!(job.steps[2].detail, "mtg-2.2.8-linux-amd64.tar.gz (v2.2.8)");
    let report = job.report.unwrap();
    assert!(report.active);
    assert!(report.status.contains("mtg.service - fake status"));

    assert_eq!(fake.read("/usr/local/bin/mtg"), BINARY);
    let unit = String::from_utf8(fake.read("/etc/systemd/system/mtg.service")).unwrap();
    assert!(
        unit.contains(&format!(
            "\nExecStart=/usr/local/bin/mtg simple-run 0.0.0.0:443 {SECRET}\n"
        )),
        "{unit}"
    );
    let log = String::from_utf8(fake.read("systemctl.log")).unwrap();
    for call in [
        "stop mtg",
        "daemon-reload",
        "enable mtg",
        "restart mtg",
        "is-active mtg",
    ] {
        assert!(
            log.lines().any(|l| l == call),
            "missing `{call}` in:\n{log}"
        );
    }
}

#[tokio::test]
async fn saves_the_confirmed_host_key_after_login() {
    let fake = Fake::new("hostkey", Server::default());
    let port = common::start(Login::Password, &fake.prelude()).await;
    let tracker = Tracker::default();
    tracker.start().unwrap();
    fake.deployer().run(request(port), &tracker).await;

    let keys = HostKeys::new(fake.root.join("data/known_hosts.json"));
    assert_eq!(
        keys.status("127.0.0.1", port, HOST_FINGERPRINT),
        KeyStatus::Known
    );
}

#[tokio::test]
async fn deploys_as_root_without_sudo() {
    let server = Server {
        uid: 0,
        sudo_allowed: false,
        ..Server::default()
    };
    let (fake, tracker) = run("root", server, |_| {}).await;
    let job = tracker.snapshot();
    assert_eq!(job.error, None, "{job:#?}");
    assert_eq!(job.steps[1].detail, "x86_64 (amd64), systemd, root");
    assert_eq!(fake.read("/usr/local/bin/mtg"), BINARY);
}

#[tokio::test]
async fn stops_before_installing_when_sudo_is_denied() {
    let server = Server {
        sudo_allowed: false,
        ..Server::default()
    };
    let (fake, tracker) = run("nosudo", server, |_| {}).await;
    let job = tracker.snapshot();

    let error = job.error.unwrap();
    assert!(error.contains("not in the sudoers file"), "{error}");
    assert_eq!(job.steps.last().unwrap().title, "Check the server");
    assert_eq!(job.steps.last().unwrap().state, StepState::Failed);
    assert!(fake.read("/usr/local/bin/mtg").is_empty());
}

#[tokio::test]
async fn refuses_to_log_in_when_the_host_key_changed() {
    let (fake, tracker) = run("mismatch", Server::default(), |r| {
        r.fingerprint = "SHA256:confirmed-earlier".into();
    })
    .await;
    let job = tracker.snapshot();

    assert!(job.error.unwrap().contains("refusing to continue"));
    assert_eq!(job.steps.len(), 1);
    assert!(fake.read("/usr/local/bin/mtg").is_empty());
}

#[tokio::test]
async fn reports_a_service_that_does_not_stay_up() {
    let server = Server {
        service_starts: false,
        ..Server::default()
    };
    let (_fake, tracker) = run("down", server, |_| {}).await;
    let job = tracker.snapshot();

    assert_eq!(
        job.error.as_deref(),
        Some("mtg.service is not running; see its status below")
    );
    let report = job.report.unwrap();
    assert!(!report.active);
    assert!(report.status.contains("fake status"));
    assert_eq!(job.steps.last().unwrap().state, StepState::Failed);
}

#[test]
fn validates_the_form_before_connecting() {
    let target = Target::default();
    let ok = request(22);
    assert_eq!(ok.validate(&target), Ok(()));

    type Edit = fn(&mut Request);
    let bad: [(&str, Edit); 6] = [
        ("host", |r| r.host = "bad host".into()),
        ("port", |r| r.ssh_port = 0),
        ("user", |r| r.username = "a b".into()),
        ("password", |r| r.password.clear()),
        ("fingerprint", |r| r.fingerprint.clear()),
        ("secret", |r| r.secret = "ee$(reboot)".into()),
    ];
    for (what, edit) in bad {
        let mut req = ok.clone();
        edit(&mut req);
        assert!(req.validate(&target).is_err(), "accepted bad {what}");
    }
}

#[test]
fn only_one_job_runs_at_a_time() {
    let tracker = Tracker::default();
    assert_eq!(tracker.start(), Some(1));
    assert_eq!(tracker.start(), None);
}
