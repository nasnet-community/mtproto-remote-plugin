# syntax=docker/dockerfile:1

# Two images: MTProto Remote is the default (the last stage), Local MTProto is --target local.

# the router's architecture, for the stage below that picks a telemt build
ARG TARGETARCH

# The web page, built into one self-contained index.html, embedded brotli-compressed.
FROM --platform=$BUILDPLATFORM node:22-alpine AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build && node -e "const z=require('zlib'),f=require('fs');f.writeFileSync('dist/index.html.br',z.brotliCompressSync(f.readFileSync('dist/index.html'),{params:{[z.constants.BROTLI_PARAM_QUALITY]:11,[z.constants.BROTLI_PARAM_MODE]:z.constants.BROTLI_MODE_TEXT}}))"

# Local MTProto's page: the same code, built for one proxy on the router itself.
FROM web AS web-local
RUN PLUGIN=local npx vite build && node -e "const z=require('zlib'),f=require('fs');f.writeFileSync('dist-local/index.html.br',z.brotliCompressSync(f.readFileSync('dist-local/index.html'),{params:{[z.constants.BROTLI_PARAM_QUALITY]:11,[z.constants.BROTLI_PARAM_MODE]:z.constants.BROTLI_MODE_TEXT}}))"

# Static binary, cross-compiled on the build platform so nothing runs under QEMU.
FROM --platform=$BUILDPLATFORM golang:1.27.1-alpine AS build
ARG TARGETOS TARGETARCH TARGETVARIANT
ARG VERSION=0.0.0-dev
# upx packs foreign-arch ELF files too, so it runs on the build platform as well
RUN apk add --no-cache ca-certificates upx
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /web/dist/index.html.br internal/plugin/ui/index.html.br
# HTTP/1.1 is enough: plain HTTP behind the panel's proxy, and the client only downloads files
RUN set -eux; \
    if [ "$TARGETARCH" = "arm" ] && [ -n "$TARGETVARIANT" ]; then export GOARM=${TARGETVARIANT#v}; fi; \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
      go build -trimpath -buildvcs=false -tags=netgo,nethttpomithttp2 -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/mtproto-remote ./cmd/mtproto-remote; \
    stat -c %s /out/mtproto-remote > /out/unpacked-size; \
    upx -q --lzma /out/mtproto-remote; \
    upx -q -t /out/mtproto-remote

# The packed binary and its unpacked size, so CI can export both and check them.
FROM scratch AS binary
COPY --from=build /out/mtproto-remote /out/unpacked-size /

# --- Local MTProto: its binary, and telemt ---

FROM --platform=$BUILDPLATFORM golang:1.27.1-alpine AS build-local
ARG TARGETOS TARGETARCH TARGETVARIANT
ARG VERSION=0.0.0-dev
RUN apk add --no-cache ca-certificates upx
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web-local /web/dist-local/index.html.br internal/local/page/index.html.br
RUN set -eux; \
    if [ "$TARGETARCH" = "arm" ] && [ -n "$TARGETVARIANT" ]; then export GOARM=${TARGETVARIANT#v}; fi; \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
      go build -trimpath -buildvcs=false -tags=netgo,nethttpomithttp2 -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/mtproto-local ./cmd/mtproto-local; \
    stat -c %s /out/mtproto-local > /out/unpacked-size; \
    upx -q --lzma /out/mtproto-local; \
    upx -q -t /out/mtproto-local

FROM scratch AS local-binary
COPY --from=build-local /out/mtproto-local /out/unpacked-size /

# telemt for 64-bit routers: telemt's own release, checked against the pins in release/release.go.
FROM --platform=$BUILDPLATFORM alpine:3.22 AS telemt-release
ARG TARGETARCH
ARG TELEMT_VERSION=3.4.25
ARG TELEMT_COMMIT=d851200e47e56b542f936c154945e75e42b671eb
RUN set -eux; \
    case "$TARGETARCH" in \
      amd64) f=telemt-x86_64-linux-musl.tar.gz sum=33e21f17d6b54111ac9d53d85d54f191cf1e6c48cc9234ce05de5fe1bd8c5de8 ;; \
      arm64) f=telemt-aarch64-linux-musl.tar.gz sum=7c9a2196418bc9ecc8c7e493e9cfb299410cbbc065920fe3115ba08109118eed ;; \
      *) echo "telemt publishes no release for $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    wget -qO /tmp/telemt.tgz "https://github.com/telemt/telemt/releases/download/$TELEMT_VERSION/$f"; \
    echo "$sum  /tmp/telemt.tgz" | sha256sum -c -; \
    mkdir /out; tar -xzf /tmp/telemt.tgz -C /out telemt; \
    wget -qO /out/LICENSE "https://raw.githubusercontent.com/telemt/telemt/$TELEMT_COMMIT/LICENSE"; \
    echo "c18e6fce5574fb3c5a19c38e2cc875e9101172c98a67aa1b87004752325994e1  /out/LICENSE" | sha256sum -c -; \
    : > /out/BUILD
FROM telemt-release AS telemt-amd64
FROM telemt-release AS telemt-arm64

# 32-bit ARM: telemt publishes no build for it, so this one comes from the same release's
# unchanged source, built with the flags of telemt's own release builds.
FROM --platform=$BUILDPLATFORM messense/rust-musl-cross:armv7-musleabihf@sha256:714d7529ed9098699cc13abade83c565e6a946e53975c108089a8cd3b43cb871 AS telemt-arm
ARG TELEMT_VERSION=3.4.25
ARG TELEMT_COMMIT=d851200e47e56b542f936c154945e75e42b671eb
WORKDIR /telemt
RUN git clone --quiet --depth 1 --branch "$TELEMT_VERSION" https://github.com/telemt/telemt.git . && test "$(git rev-parse HEAD)" = "$TELEMT_COMMIT"
RUN --mount=type=cache,id=telemt-armv7-registry,target=/root/.cargo/registry \
    --mount=type=cache,id=telemt-armv7-target,target=/telemt/target \
    set -eux; \
    RUSTFLAGS="-C target-feature=+crt-static -C lto=fat -C panic=abort" \
      cargo build --locked --release --target armv7-unknown-linux-musleabihf; \
    mkdir /out; \
    cp target/armv7-unknown-linux-musleabihf/release/telemt /out/telemt; \
    armv7-unknown-linux-musleabihf-strip /out/telemt; \
    cp LICENSE /out/LICENSE; \
    printf "Unofficial build of telemt %s for 32-bit ARM, made by NASNET Community from telemt's unchanged source (commit %s): telemt publishes no build for it.\n" "$TELEMT_VERSION" "$TELEMT_COMMIT" > /out/BUILD

# the telemt build for the router's architecture
FROM telemt-${TARGETARCH} AS telemt

# Local MTProto: the plugin runs telemt beside it in one container. Root, as for Remote;
# telemt itself drops to nobody.
FROM busybox:musl AS local
COPY --from=build-local /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=local-binary /mtproto-local /mtproto-local
COPY --from=telemt /out/telemt /usr/local/bin/telemt
COPY --from=telemt /out/LICENSE /out/BUILD /usr/local/share/telemt/
ENV HOST=0.0.0.0 PORT=8080 DATA_DIR=/data BASE_PATH= TELEMT_BIN=/usr/local/bin/telemt \
    GOMAXPROCS=2 GOMEMLIMIT=40MiB SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt
EXPOSE 8080 8443
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD wget -qO- http://127.0.0.1:8080/health || exit 1
ENTRYPOINT ["/mtproto-local"]

# --- MTProto Remote ---

# busybox for a shell and wget (the healthcheck), CA roots for HTTPS.
# Root because RouterOS mounts are root-owned (check on a device before adding USER).
FROM busybox:musl
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=binary /mtproto-remote /mtproto-remote
ENV HOST=0.0.0.0 PORT=8080 DATA_DIR=/data BASE_PATH= \
    GOMAXPROCS=2 GOMEMLIMIT=60MiB SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD wget -qO- http://127.0.0.1:8080/health || exit 1
ENTRYPOINT ["/mtproto-remote"]
