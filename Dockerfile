# syntax=docker/dockerfile:1

# The web page, built into one self-contained index.html, embedded gzipped.
FROM --platform=$BUILDPLATFORM node:22-alpine AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build && gzip -9 -c < dist/index.html > dist/index.html.gz

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
COPY --from=web /web/dist/index.html.gz internal/plugin/ui/index.html.gz
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
