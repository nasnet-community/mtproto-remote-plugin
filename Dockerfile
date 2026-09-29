# syntax=docker/dockerfile:1

# Static musl binary, cross-compiled from the build platform with zig so the
# build never runs under QEMU emulation.
FROM --platform=$BUILDPLATFORM ghcr.io/rust-cross/cargo-zigbuild:latest AS build
WORKDIR /app
ARG TARGETARCH
RUN case "${TARGETARCH}" in \
      amd64) echo x86_64-unknown-linux-musl ;; \
      arm64) echo aarch64-unknown-linux-musl ;; \
      *) echo "unsupported TARGETARCH: ${TARGETARCH}" >&2; exit 1 ;; \
    esac > /rust-target \
    && rustup target add "$(cat /rust-target)"

# Cache dependency compilation against dummy sources.
COPY Cargo.toml Cargo.lock ./
RUN mkdir src \
    && echo 'fn main() {}' > src/main.rs \
    && touch src/lib.rs \
    && cargo zigbuild --release --locked --target "$(cat /rust-target)" \
    && rm -rf src

COPY src ./src
RUN touch src/* \
    && cargo zigbuild --release --locked --target "$(cat /rust-target)" \
    && cp "target/$(cat /rust-target)/release/mtproto-remote" /mtproto-remote

# TLS roots are compiled in; the image holds nothing but the binary.
FROM scratch
COPY --from=build /mtproto-remote /mtproto-remote
ENV HOST=0.0.0.0 \
    PORT=8080 \
    DATA_DIR=/data \
    BASE_PATH=
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/mtproto-remote"]
