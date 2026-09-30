# syntax=docker/dockerfile:1

# The web page, built into one self-contained index.html.
FROM --platform=$BUILDPLATFORM node:22-alpine AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

# Static musl binary, cross-compiled from the build platform with zig so the
# build never runs under QEMU emulation. build.rs embeds the page.
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

COPY build.rs ./
COPY src ./src
COPY --from=web /web/dist ./web/dist
RUN touch build.rs src/* \
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
