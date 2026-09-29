//! Installing telemt end to end against the fake server (see common/fake.rs).

mod common;

use common::fake::{BINARY, Fake, Options};
use common::{HOST_FINGERPRINT, USER};
use mtproto_remote::deploy::{Job, StepState, Tracker};
use mtproto_remote::keys::authorized_line;

async fn install(fake: &Fake, edit: impl FnOnce(&mut mtproto_remote::deploy::Request)) -> Job {
    let mut req = fake.request();
    edit(&mut req);
    let tracker = Tracker::default();
    tracker.start().unwrap();
    fake.deployer().run(req, &tracker).await;
    tracker.snapshot()
}

fn assert_all_done(job: &Job) {
    assert_eq!(job.error, None, "{job:#?}");
    assert!(!job.running);
    assert!(
        job.steps.iter().all(|s| s.state == StepState::Done),
        "{job:#?}"
    );
}

#[tokio::test]
async fn installs_through_sudo_and_hands_over_to_the_router_key() {
    let fake = Fake::new("sudo", Options::default()).await;
    let job = install(&fake, |_| {}).await;
    assert_all_done(&job);
    assert_eq!(job.steps[1].detail, "x86_64, systemd, sudo");
    let report = job.report.unwrap();
    assert!(!report.kept_config);
    assert_eq!((report.users, report.release.as_str()), (1, "3.5.9"));

    // Files in place.
    assert!(
        fake.read("/usr/local/bin/telemt")
            .starts_with(std::str::from_utf8(BINARY).unwrap())
    );
    let conf = fake.read("/etc/telemt/telemt.toml");
    assert!(conf.contains("\n\"first\" = \""), "{conf}");
    assert!(conf.contains("\npublic_host = \"127.0.0.1\"\npublic_port = 443\n"));
    assert!(
        fake.read("/etc/systemd/system/telemt.service")
            .contains("\nUser=telemt\n")
    );
    assert!(
        fake.read("/usr/local/sbin/nasnet-telemt-ctl")
            .starts_with("#!/bin/sh")
    );
    let rule = fake.read("/etc/sudoers.d/nasnet-telemt");
    assert!(rule.contains(&format!(
        "\n{USER} ALL=(root) NOPASSWD: {}\n",
        fake.layout.ctl()
    )));
    assert_eq!(fake.read("/users"), "telemt\n");

    // The router key was added and the server saved with the config's token.
    assert!(fake.authorized_keys().contains(&authorized_line(&fake.key)));
    let servers = fake.store.list();
    assert_eq!(servers.len(), 1);
    let server = &servers[0];
    assert_eq!(server.id, report.server_id);
    assert_eq!((server.name.as_str(), server.is_root), ("Test box", false));
    assert_eq!(server.fingerprint, HOST_FINGERPRINT);
    assert!(conf.contains(&format!("auth_header = \"Bearer {}\"", server.api_token)));

    // Temp uploads were cleaned up.
    let leftovers: Vec<_> = std::fs::read_dir("/tmp")
        .unwrap()
        .filter_map(|e| e.ok())
        .filter(|e| {
            e.file_name()
                .to_string_lossy()
                .starts_with("nasnet-telemt.")
        })
        .collect();
    assert!(leftovers.is_empty(), "{leftovers:?}");
}

#[tokio::test]
async fn installs_as_root_without_a_sudoers_rule() {
    let fake = Fake::new(
        "root",
        Options {
            uid: 0,
            sudo_allowed: false,
            ..Options::default()
        },
    )
    .await;
    let job = install(&fake, |_| {}).await;
    assert_all_done(&job);
    assert_eq!(job.steps[1].detail, "x86_64, systemd, root");
    assert!(!fake.exists("/etc/sudoers.d/nasnet-telemt"));
    assert!(fake.store.list()[0].is_root);
}

#[tokio::test]
async fn reinstalling_keeps_the_existing_config_and_users() {
    let fake = Fake::new("reinstall", Options::default()).await;
    let first = install(&fake, |_| {}).await;
    assert_all_done(&first);
    let conf = fake.read("/etc/telemt/telemt.toml");
    let token = fake.store.list()[0].api_token.clone();

    let again = install(&fake, |r| r.tls_domain = "www.example.org".into()).await;
    assert_all_done(&again);
    assert!(again.report.unwrap().kept_config);
    assert_eq!(fake.read("/etc/telemt/telemt.toml"), conf);
    let servers = fake.store.list();
    assert_eq!(servers.len(), 1, "reinstall must not add a second entry");
    assert_eq!(servers[0].api_token, token);
    assert_eq!(
        fake.authorized_keys()
            .matches(&authorized_line(&fake.key))
            .count(),
        1,
        "the key must be added once"
    );
}

#[tokio::test]
async fn stops_before_installing_when_sudo_is_denied() {
    let fake = Fake::new(
        "nosudo",
        Options {
            sudo_allowed: false,
            ..Options::default()
        },
    )
    .await;
    let job = install(&fake, |_| {}).await;
    assert!(job.error.unwrap().contains("not in the sudoers file"));
    let last = job.steps.last().unwrap();
    assert_eq!(
        (last.title.as_str(), last.state),
        ("Check the server", StepState::Failed)
    );
    assert!(!fake.exists("/usr/local/bin/telemt"));
    assert!(fake.store.list().is_empty());
}

#[tokio::test]
async fn refuses_to_log_in_when_the_host_key_changed() {
    let fake = Fake::new("mismatch", Options::default()).await;
    let job = install(&fake, |r| r.fingerprint = "SHA256:confirmed-earlier".into()).await;
    assert!(job.error.unwrap().contains("refusing to continue"));
    assert_eq!(job.steps.len(), 1);
    assert!(!fake.exists("/usr/local/bin/telemt"));
}

#[tokio::test]
async fn does_not_save_a_server_whose_api_never_answers() {
    let fake = Fake::new(
        "noapi",
        Options {
            api_up: false,
            ..Options::default()
        },
    )
    .await;
    let job = install(&fake, |_| {}).await;
    let error = job.error.unwrap();
    assert!(
        error.contains("the proxy did not start answering"),
        "{error}"
    );
    assert_eq!(
        job.steps.last().unwrap().title,
        "Wait for the proxy to answer"
    );
    assert!(fake.store.list().is_empty());
}

#[tokio::test]
async fn validates_the_form_before_connecting() {
    let fake = Fake::new("validate", Options::default()).await;
    let ok = fake.request();
    assert_eq!(ok.validate(), Ok(()));
    type Edit = fn(&mut mtproto_remote::deploy::Request);
    let bad: [(&str, Edit); 8] = [
        ("host", |r| r.host = "bad host".into()),
        ("port", |r| r.ssh_port = 0),
        ("user", |r| r.username = "a b".into()),
        ("password", |r| r.password.clear()),
        ("fingerprint", |r| r.fingerprint.clear()),
        ("proxy port", |r| r.proxy_port = 0),
        ("domain", |r| r.tls_domain = "localhost".into()),
        ("first user", |r| r.first_user = "bad user".into()),
    ];
    for (what, edit) in bad {
        let mut req = ok.clone();
        edit(&mut req);
        assert!(req.validate().is_err(), "accepted bad {what}");
    }
}

#[test]
fn only_one_job_runs_at_a_time() {
    let tracker = Tracker::default();
    assert_eq!(tracker.start(), Some(1));
    assert_eq!(tracker.start(), None);
}
