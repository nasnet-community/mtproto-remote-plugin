//! Installing telemt on a new server, step by step, with progress the page polls.

use std::fmt::Display;
use std::sync::{Arc, Mutex};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use russh::keys::PrivateKey;
use serde::{Deserialize, Serialize};

use crate::config::Target;
use crate::hostkeys::HostKeys;
use crate::keys::{authorized_line, random_hex};
use crate::release::{self, ASSET_PATTERN, BINARY, GitHub, ReleaseError};
use crate::servers::{Server, ServerStore};
use crate::ssh::Session;
use crate::telemt::{
    Layout, NewConfig, Remote, Uploads, config_toml, ctl_script, is_host, is_simple_name,
    sudoers_rule, unit_file,
};

/// What the add-server form submits.
#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Request {
    pub host: String,
    pub ssh_port: u16,
    pub username: String,
    pub password: String,
    /// The host key fingerprint the user confirmed.
    pub fingerprint: String,
    #[serde(default)]
    pub name: String,
    pub proxy_port: u16,
    pub tls_domain: String,
    pub first_user: String,
}

pub fn validate_host(host: &str) -> Result<(), String> {
    if is_host(host) {
        Ok(())
    } else {
        Err("Enter a server hostname or IP address.".into())
    }
}

impl Request {
    pub fn normalize(&mut self) {
        for field in [
            &mut self.host,
            &mut self.username,
            &mut self.name,
            &mut self.tls_domain,
            &mut self.first_user,
        ] {
            *field = field.trim().to_string();
        }
        if self.name.is_empty() {
            self.name = self.host.clone();
        }
    }

    pub fn validate(&self) -> Result<(), String> {
        validate_host(&self.host)?;
        if self.ssh_port == 0 {
            return Err("Enter the SSH port.".into());
        }
        if !is_simple_name(&self.username, 32) {
            return Err("Enter a valid SSH username.".into());
        }
        if self.password.is_empty() {
            return Err("Enter the SSH password.".into());
        }
        if !self.fingerprint.starts_with("SHA256:") {
            return Err("Confirm the server's host key first.".into());
        }
        if self.name.chars().count() > 64 {
            return Err("Keep the server name to 64 characters.".into());
        }
        if self.proxy_port == 0 {
            return Err("Enter the proxy port.".into());
        }
        if !is_host(&self.tls_domain) || !self.tls_domain.contains('.') {
            return Err("Enter a domain to disguise traffic as, like www.google.com.".into());
        }
        if !is_simple_name(&self.first_user, 64) {
            return Err("Name the first user with letters, digits, '.', '_' or '-'.".into());
        }
        Ok(())
    }
}

/// The telemt executable, unpacked.
pub struct Fetched {
    pub tag: String,
    pub asset: String,
    pub binary: Vec<u8>,
}

/// Where telemt comes from. Blocking; it is called on a blocking thread.
pub trait Source: Send + Sync {
    fn fetch(&self, target: &Target, arch: &str) -> Result<Fetched, ReleaseError>;
}

#[derive(Default)]
pub struct GitHubSource(pub GitHub);

impl Source for GitHubSource {
    fn fetch(&self, target: &Target, arch: &str) -> Result<Fetched, ReleaseError> {
        let rel = self.0.release(target)?;
        let asset = release::select_asset(&rel, ASSET_PATTERN, arch)?;
        let bytes = self.0.download(asset)?;
        Ok(Fetched {
            binary: release::extract_binary(&asset.name, &bytes, BINARY)?,
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
    pub server_id: String,
    pub release: String,
    pub kept_config: bool,
    pub users: usize,
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

    pub fn begin(&self, title: impl Into<String>) {
        self.0.lock().expect("job lock").steps.push(Step {
            title: title.into(),
            state: StepState::Running,
            detail: String::new(),
        });
    }

    pub fn done(&self, detail: impl Into<String>) {
        if let Some(step) = self.0.lock().expect("job lock").steps.last_mut() {
            step.state = StepState::Done;
            step.detail = detail.into();
        }
    }

    /// Marks the current step failed and returns the message.
    pub fn fail(&self, err: impl Display) -> String {
        let message = err.to_string();
        if let Some(step) = self.0.lock().expect("job lock").steps.last_mut() {
            step.state = StepState::Failed;
            step.detail = message.clone();
        }
        message
    }

    pub fn finish(&self, result: Result<Report, String>) {
        let mut job = self.0.lock().expect("job lock");
        job.running = false;
        match result {
            Ok(report) => job.report = Some(report),
            Err(error) => job.error = Some(error),
        }
    }
}

pub struct Deployer {
    pub target: Target,
    pub source: Arc<dyn Source>,
    pub host_keys: HostKeys,
    pub layout: Layout,
    pub key: Arc<PrivateKey>,
    pub store: Arc<ServerStore>,
    /// How long to wait for telemt's API after starting it.
    pub api_wait: Duration,
}

fn now() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |d| d.as_secs())
}

impl Deployer {
    /// Runs the whole install and records the outcome in `tracker`.
    pub async fn run(&self, req: Request, tracker: &Tracker) {
        tracker.finish(self.deploy(&req, tracker).await);
    }

    async fn deploy(&self, req: &Request, t: &Tracker) -> Result<Report, String> {
        t.begin(format!("Connect to {}:{}", req.host, req.ssh_port));
        let session = Session::connect(
            &req.host,
            req.ssh_port,
            &req.fingerprint,
            &req.username,
            &req.password,
        )
        .await
        .map_err(|e| t.fail(e))?;
        let _ = self
            .host_keys
            .save(&req.host, req.ssh_port, &req.fingerprint);
        t.done(format!("Logged in as {}", req.username));

        let result = self.deploy_on(&session, req, t).await;
        session.close().await;
        result
    }

    async fn deploy_on(
        &self,
        session: &Session,
        req: &Request,
        t: &Tracker,
    ) -> Result<Report, String> {
        t.begin("Check the server");
        let (remote, info) = Remote::prepare(session, &self.layout, &req.password)
            .await
            .map_err(|e| t.fail(e))?;
        let arch = release::release_arch(&info.uname_m).map_err(|e| t.fail(e))?;
        t.done(format!(
            "{arch}, systemd, {}",
            if info.is_root { "root" } else { "sudo" }
        ));

        t.begin(format!("Download telemt from {}", self.target.repo));
        let (source, target) = (self.source.clone(), self.target.clone());
        let fetched = tokio::task::spawn_blocking(move || source.fetch(&target, arch))
            .await
            .map_err(|e| t.fail(e))?
            .map_err(|e| t.fail(e))?;
        t.done(format!(
            "{} ({}, {:.1} MB)",
            fetched.asset,
            fetched.tag,
            fetched.binary.len() as f64 / 1e6
        ));

        let mut temps = Vec::new();
        let installed = self
            .upload_and_install(&remote, req, &info, &fetched, &mut temps, t)
            .await;
        remote.remove_temp(&temps).await;
        let installed = installed?;

        t.begin("Give the router its own key");
        remote
            .authorize_key(&authorized_line(&self.key))
            .await
            .map_err(|e| t.fail(e))?;
        let managed = Session::connect_with_key(
            &req.host,
            req.ssh_port,
            &req.fingerprint,
            &req.username,
            &self.key,
        )
        .await
        .map_err(|e| t.fail(e))?;
        t.done("Key login works; the password is not kept");

        t.begin("Wait for telemt to answer");
        let users = self.wait_for_api(&managed, &installed.api_token).await;
        managed.close().await;
        let users = users.map_err(|e| t.fail(e))?;
        t.done(format!("{users} user(s)"));

        t.begin("Save the server");
        let server = self
            .store
            .upsert(Server {
                id: random_hex::<8>().map_err(|e| t.fail(e))?,
                name: req.name.clone(),
                host: req.host.clone(),
                ssh_port: req.ssh_port,
                username: req.username.clone(),
                fingerprint: req.fingerprint.clone(),
                is_root: info.is_root,
                api_token: installed.api_token,
                proxy_port: req.proxy_port,
                tls_domain: req.tls_domain.clone(),
                arch: arch.to_string(),
                added_at: now(),
            })
            .map_err(|e| t.fail(e))?;
        t.done(server.name.clone());

        Ok(Report {
            server_id: server.id,
            release: fetched.tag,
            kept_config: installed.kept_config,
            users,
        })
    }

    async fn upload_and_install(
        &self,
        remote: &Remote<'_>,
        req: &Request,
        info: &crate::telemt::ServerInfo,
        fetched: &Fetched,
        temps: &mut Vec<String>,
        t: &Tracker,
    ) -> Result<crate::telemt::Installed, String> {
        t.begin("Upload telemt and its files");
        let config = config_toml(
            &self.layout,
            &NewConfig {
                public_host: req.host.clone(),
                port: req.proxy_port,
                tls_domain: req.tls_domain.clone(),
                first_user: req.first_user.clone(),
                first_secret: random_hex::<16>().map_err(|e| t.fail(e))?,
                api_token: random_hex::<24>().map_err(|e| t.fail(e))?,
            },
        );
        let mut upload = async |what: &str, bytes: &[u8]| {
            let path = remote.upload(what, bytes).await?;
            temps.push(path.clone());
            Ok::<_, crate::telemt::RemoteError>(path)
        };
        let uploads = async {
            Ok::<_, crate::telemt::RemoteError>(Uploads {
                binary: upload("Uploading telemt", &fetched.binary).await?,
                config: upload("Uploading the config", config.as_bytes()).await?,
                ctl: upload("Uploading the helper", ctl_script(&self.layout).as_bytes()).await?,
                unit: upload(
                    "Uploading the unit",
                    unit_file(&self.layout, &fetched.tag).as_bytes(),
                )
                .await?,
                sudoers: match info.is_root {
                    true => None,
                    false => Some(
                        upload(
                            "Uploading the sudoers rule",
                            sudoers_rule(&self.layout, &req.username).as_bytes(),
                        )
                        .await?,
                    ),
                },
            })
        }
        .await
        .map_err(|e| t.fail(e))?;
        t.done("");

        t.begin("Install and start telemt");
        let installed = remote.install(&uploads).await.map_err(|e| t.fail(e))?;
        t.done(if installed.kept_config {
            "Kept the existing configuration and its users"
        } else {
            "New configuration"
        });
        Ok(installed)
    }

    /// Polls `GET /v1/users` until telemt answers; returns the user count.
    async fn wait_for_api(&self, session: &Session, token: &str) -> Result<usize, String> {
        let auth = format!("Bearer {token}");
        let deadline = tokio::time::Instant::now() + self.api_wait;
        loop {
            let last = match session
                .http(self.layout.api_port, "GET", "/v1/users", Some(&auth), None)
                .await
            {
                Ok(res) if res.status == 200 => {
                    let body: serde_json::Value =
                        serde_json::from_slice(&res.body).map_err(|e| e.to_string())?;
                    return Ok(body["data"].as_array().map_or(0, Vec::len));
                }
                Ok(res) => format!(
                    "telemt answered {}: {}",
                    res.status,
                    String::from_utf8_lossy(&res.body)
                ),
                Err(e) => e.to_string(),
            };
            if tokio::time::Instant::now() >= deadline {
                return Err(format!(
                    "telemt's API did not come up ({last}). Check the service logs on the server."
                ));
            }
            tokio::time::sleep(Duration::from_millis(500)).await;
        }
    }
}
