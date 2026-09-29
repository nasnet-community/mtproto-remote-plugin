use std::sync::Arc;
use std::time::Duration;

use mtproto_remote::deploy::{Deployer, GitHubSource};
use mtproto_remote::hostkeys::HostKeys;
use mtproto_remote::{config, web};
use tokio::net::TcpListener;
use tokio::signal::unix::{SignalKind, signal};

#[tokio::main]
async fn main() {
    let cfg = config::from_env().unwrap_or_else(|err| {
        eprintln!("config error: {err}");
        std::process::exit(1);
    });

    let deployer = Deployer {
        target: cfg.target.clone(),
        source: Arc::new(GitHubSource::default()),
        host_keys: HostKeys::new(cfg.data_dir.join("known_hosts.json")),
        settle: Duration::from_secs(3),
    };
    let app = web::router(web::AppState::new(deployer, &cfg.base_path), &cfg.base_path);

    let listener = TcpListener::bind((cfg.host.as_str(), cfg.port))
        .await
        .unwrap_or_else(|err| {
            eprintln!("cannot listen on {}:{}: {err}", cfg.host, cfg.port);
            std::process::exit(1);
        });
    println!(
        "mtproto-remote {} listening on {}:{}{} (deploys {})",
        env!("CARGO_PKG_VERSION"),
        cfg.host,
        cfg.port,
        cfg.base_path,
        cfg.target.repo,
    );

    let shutdown = async {
        let mut term = signal(SignalKind::terminate()).expect("SIGTERM handler");
        tokio::select! {
            _ = term.recv() => {}
            _ = tokio::signal::ctrl_c() => {}
        }
    };
    if let Err(err) = axum::serve(listener, app)
        .with_graceful_shutdown(shutdown)
        .await
    {
        eprintln!("server error: {err}");
        std::process::exit(1);
    }
}
