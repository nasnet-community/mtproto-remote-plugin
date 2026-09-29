use std::sync::Arc;
use std::time::Duration;

use mtproto_remote::deploy::{Deployer, GitHubSource, Source};
use mtproto_remote::hostkeys::HostKeys;
use mtproto_remote::manage::Manager;
use mtproto_remote::servers::ServerStore;
use mtproto_remote::telemt::Layout;
use mtproto_remote::{config, keys, web};
use tokio::net::TcpListener;
use tokio::signal::unix::{SignalKind, signal};

fn fail(message: String) -> ! {
    eprintln!("{message}");
    std::process::exit(1);
}

#[tokio::main]
async fn main() {
    let cfg = config::from_env().unwrap_or_else(|e| fail(format!("config error: {e}")));

    let key_path = cfg.data_dir.join("id_ed25519");
    let key =
        Arc::new(keys::load_or_create(&key_path).unwrap_or_else(|e| {
            fail(format!("cannot load or create {}: {e}", key_path.display()))
        }));
    let store_path = cfg.data_dir.join("servers.json");
    let store = Arc::new(
        ServerStore::open(&store_path)
            .unwrap_or_else(|e| fail(format!("cannot read {}: {e}", store_path.display()))),
    );
    let source: Arc<dyn Source> = Arc::new(GitHubSource::default());
    let layout = Layout::default();

    let deployer = Deployer {
        target: cfg.target.clone(),
        source: source.clone(),
        host_keys: HostKeys::new(cfg.data_dir.join("known_hosts.json")),
        layout: layout.clone(),
        key: key.clone(),
        store: store.clone(),
        api_wait: Duration::from_secs(20),
    };
    let manager = Manager::new(store, key, layout, cfg.target.clone(), source);
    let state = web::AppState::new(deployer, manager, &cfg.base_path);
    let app = web::router(state, &cfg.base_path);

    let listener = TcpListener::bind((cfg.host.as_str(), cfg.port))
        .await
        .unwrap_or_else(|e| fail(format!("cannot listen on {}:{}: {e}", cfg.host, cfg.port)));
    println!(
        "mtproto-remote {} listening on {}:{}{} (installs {}, data in {})",
        env!("CARGO_PKG_VERSION"),
        cfg.host,
        cfg.port,
        cfg.base_path,
        cfg.target.repo,
        cfg.data_dir.display(),
    );

    let shutdown = async {
        let mut term = signal(SignalKind::terminate()).expect("SIGTERM handler");
        tokio::select! {
            _ = term.recv() => {}
            _ = tokio::signal::ctrl_c() => {}
        }
    };
    if let Err(e) = axum::serve(listener, app)
        .with_graceful_shutdown(shutdown)
        .await
    {
        fail(format!("server error: {e}"));
    }
}
