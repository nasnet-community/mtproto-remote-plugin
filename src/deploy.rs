//! One deployment, run step by step, with progress the UI can poll.

use std::fmt::Display;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use serde::{Deserialize, Serialize};

use crate::config::Target;
use crate::hostkeys::HostKeys;
use crate::install::{self, Remote, is_plain_arg, render_exec_args, unit_file};
use crate::release::{self, GitHub, ReleaseError};
use crate::ssh::Session;

/// What the form submits.
#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Request {
    pub host: String,
    pub ssh_port: u16,
    pub username: String,
    pub password: String,
    /// The host key fingerprint the user confirmed.
    pub fingerprint: String,
    pub proxy_port: u16,
    pub secret: String,
}

pub fn validate_host(host: &str) -> Result<(), String> {
    let ok = !host.is_empty()
        && host.len() <= 253
        && host
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, '.' | '-' | ':'));
    if ok {
        Ok(())
    } else {
        Err("Enter a server hostname or IP address.".into())
    }
}

impl Request {
    pub fn validate(&self, target: &Target) -> Result<(), String> {
        validate_host(&self.host)?;
        if self.ssh_port == 0 {
            return Err("Enter the SSH port.".into());
        }
        if self.username.is_empty() || !is_plain_arg(&self.username) {
            return Err("Enter a valid SSH username.".into());
        }
        if self.password.is_empty() {
            return Err("Enter the SSH password.".into());
        }
        if !self.fingerprint.starts_with("SHA256:") {
            return Err("Confirm the server's host key first.".into());
        }
        if target.exec_args.contains("{port}") && self.proxy_port == 0 {
            return Err("Enter the proxy port.".into());
        }
        if target.exec_args.contains("{secret}") && !is_plain_arg(&self.secret) {
            return Err(
                "Enter a secret made of letters, digits and - _ . = + / : , @ only.".into(),
            );
        }
        Ok(())
    }
}

/// The executable to deploy, already unpacked.
pub struct Fetched {
    pub tag: String,
    pub asset: String,
    pub binary: Vec<u8>,
}

/// Where binaries come from. Blocking; it is called on a blocking thread.
pub trait Source: Send + Sync {
    fn fetch(&self, target: &Target, arch: &str) -> Result<Fetched, ReleaseError>;
}

#[derive(Default)]
pub struct GitHubSource(pub GitHub);

impl Source for GitHubSource {
    fn fetch(&self, target: &Target, arch: &str) -> Result<Fetched, ReleaseError> {
        let rel = self.0.release(target)?;
        let asset = release::select_asset(&rel, &target.asset_pattern, arch)?;
        let bytes = self.0.download(asset)?;
        Ok(Fetched {
            binary: release::extract_binary(&asset.name, &bytes, &target.binary_name)?,
            tag: rel.tag_name.clone(),
            asset: asset.name.clone(),
        })
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum StepState {
    Running,
    Done,
    Failed,
}

#[derive(Debug, Clone, Serialize)]
pub struct Step {
    pub title: String,
    pub state: StepState,
    pub detail: String,
}

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct Report {
    pub active: bool,
    pub status: String,
    pub release: String,
    pub asset: String,
}

#[derive(Debug, Clone, Default, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct Job {
    pub id: u64,
    pub running: bool,
    pub steps: Vec<Step>,
    pub error: Option<String>,
    pub report: Option<Report>,
}

/// Shared progress of the current job.
#[derive(Clone, Default)]
pub struct Tracker(pub Arc<Mutex<Job>>);

impl Tracker {
    pub fn snapshot(&self) -> Job {
        self.0.lock().expect("job lock").clone()
    }

    /// Starts a new job unless one is running; returns its id.
    pub fn start(&self) -> Option<u64> {
        let mut job = self.0.lock().expect("job lock");
        if job.running {
            return None;
        }
        *job = Job {
            id: job.id + 1,
            running: true,
            ..Job::default()
        };
        Some(job.id)
    }

    fn begin(&self, title: impl Into<String>) {
        self.0.lock().expect("job lock").steps.push(Step {
            title: title.into(),
            state: StepState::Running,
            detail: String::new(),
        });
    }

    fn done(&self, detail: impl Into<String>) {
        if let Some(step) = self.0.lock().expect("job lock").steps.last_mut() {
            step.state = StepState::Done;
            step.detail = detail.into();
        }
    }

    /// Marks the current step failed and returns the message.
    fn fail(&self, err: impl Display) -> String {
        let message = err.to_string();
        if let Some(step) = self.0.lock().expect("job lock").steps.last_mut() {
            step.state = StepState::Failed;
            step.detail = message.clone();
        }
        message
    }

    fn finish(&self, result: Result<Report, (String, Option<Report>)>) {
        let mut job = self.0.lock().expect("job lock");
        job.running = false;
        match result {
            Ok(report) => job.report = Some(report),
            Err((error, report)) => {
                job.error = Some(error);
                job.report = report;
            }
        }
    }
}

pub struct Deployer {
    pub target: Target,
    pub source: Arc<dyn Source>,
    pub host_keys: HostKeys,
    /// How long to let the service run before checking it.
    pub settle: Duration,
}

impl Deployer {
    /// Runs the whole deployment and records the outcome in `tracker`.
    pub async fn run(&self, req: Request, tracker: &Tracker) {
        let result = self.deploy(&req, tracker).await;
        tracker.finish(result);
    }

    async fn deploy(&self, req: &Request, t: &Tracker) -> Result<Report, (String, Option<Report>)> {
        let target = &self.target;

        t.begin(format!("Connect to {}:{}", req.host, req.ssh_port));
        let session = Session::connect(
            &req.host,
            req.ssh_port,
            &req.fingerprint,
            &req.username,
            &req.password,
        )
        .await
        .map_err(|e| (t.fail(e), None))?;
        let _ = self
            .host_keys
            .save(&req.host, req.ssh_port, &req.fingerprint);
        t.done(format!("Logged in as {}", req.username));

        let result = self.deploy_on(&session, req, t).await;
        session.close().await;
        let report = result?;
        if report.active {
            Ok(report)
        } else {
            let error = format!(
                "{}.service is not running; see its status below",
                target.service_name
            );
            Err((error, Some(report)))
        }
    }

    async fn deploy_on(
        &self,
        session: &Session,
        req: &Request,
        t: &Tracker,
    ) -> Result<Report, (String, Option<Report>)> {
        let target = &self.target;
        let fail = |e: &dyn Display| (t.fail(e), None);

        t.begin("Check the server");
        let (remote, info) = Remote::prepare(session, &req.password)
            .await
            .map_err(|e| fail(&e))?;
        let arch = release::release_arch(&info.uname_m).map_err(|e| fail(&e))?;
        t.done(format!(
            "{} ({arch}), systemd, {}",
            info.uname_m,
            if info.is_root { "root" } else { "sudo" }
        ));

        t.begin(format!("Download {} release", target.repo));
        let source = self.source.clone();
        let (target_owned, arch_owned) = (target.clone(), arch.to_string());
        let fetched = tokio::task::spawn_blocking(move || source.fetch(&target_owned, &arch_owned))
            .await
            .map_err(|e| fail(&e))?
            .map_err(|e| fail(&e))?;
        t.done(format!("{} ({})", fetched.asset, fetched.tag));

        let mut temps = Vec::new();
        let result = self
            .install(&remote, req, t, &fetched, &mut temps)
            .await
            .map_err(|e| fail(&e));
        remote.remove_temp(&temps).await;
        result?;

        t.begin(format!("Check {}.service", target.service_name));
        let (active, status) = remote
            .service_status(target, self.settle)
            .await
            .map_err(|e| fail(&e))?;
        let report = Report {
            active,
            status,
            release: fetched.tag,
            asset: fetched.asset,
        };
        if active {
            t.done("active (running)");
        } else {
            t.fail("not running");
        }
        Ok(report)
    }

    async fn install(
        &self,
        remote: &Remote<'_>,
        req: &Request,
        t: &Tracker,
        fetched: &Fetched,
        temps: &mut Vec<String>,
    ) -> Result<(), install::InstallError> {
        let target = &self.target;

        t.begin(format!("Upload {}", target.binary_name));
        let tmp = remote
            .upload("Uploading the binary", &fetched.binary)
            .await?;
        temps.push(tmp.clone());
        t.done(format!("{:.1} MB", fetched.binary.len() as f64 / 1e6));

        t.begin(format!("Install {}", install::binary_path(target)));
        remote.install_binary(target, &tmp).await?;
        t.done("");

        t.begin(format!("Start {}.service", target.service_name));
        let args = render_exec_args(&target.exec_args, req.proxy_port, &req.secret);
        let unit = unit_file(target, &fetched.tag, &args);
        let tmp = remote.upload("Uploading the unit", unit.as_bytes()).await?;
        temps.push(tmp.clone());
        remote.install_service(target, &tmp).await?;
        t.done(install::unit_path(target));
        Ok(())
    }
}
