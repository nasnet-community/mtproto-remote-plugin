fn main() {
    match mtproto_remote::config::from_env() {
        Ok(cfg) => println!("{cfg:?}"),
        Err(err) => {
            eprintln!("config error: {err}");
            std::process::exit(1);
        }
    }
}
