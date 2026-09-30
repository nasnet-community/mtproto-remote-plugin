//! Embeds the built web page (web/dist/index.html). Without a build — e.g. when
//! only running `cargo test` — a placeholder page is embedded instead.

use std::{env, fs, path::PathBuf};

const PLACEHOLDER: &str = "<!doctype html><html><head><meta charset=\"utf-8\"><title>MTProto Remote</title></head>\
<body><p>The web page was not built. Run <code>npm ci &amp;&amp; npm run build</code> in <code>web/</code>, then rebuild.</p></body></html>";

fn main() {
    let page = PathBuf::from("web/dist/index.html");
    println!("cargo:rerun-if-changed=build.rs");
    println!("cargo:rerun-if-changed={}", page.display());
    let out = PathBuf::from(env::var("OUT_DIR").expect("OUT_DIR")).join("index.html");
    let html = fs::read_to_string(&page).unwrap_or_else(|_| PLACEHOLDER.to_string());
    fs::write(out, html).expect("write embedded page");
}
