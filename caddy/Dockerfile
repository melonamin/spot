# Optional Caddy image with the Cloudflare DNS module, needed for DNS-01
# wildcard certificates when SPOT_TLS_MODE=tls-cloudflare.
# Cross-compile on the build platform; xcaddy honors GOOS/GOARCH, so multi-arch
# builds avoid QEMU emulation. buildx populates BUILDPLATFORM/TARGETOS/TARGETARCH.
# The builder's Go provides crypto/x509, so it must distrust the compromised
# eMudhra intermediates (GO-2026-6809: Go >= 1.26.10 or >= 1.27.3).
FROM --platform=$BUILDPLATFORM caddy:2.11.7-builder AS builder
ARG TARGETOS TARGETARCH
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH xcaddy build --with github.com/caddy-dns/cloudflare@v0.2.4

FROM caddy:2.11.7-alpine
# Exact-version tags are not rebuilt upstream, so refresh the CA bundle rather
# than keep whatever the base image shipped with.
RUN apk add --no-cache --upgrade ca-certificates
COPY --from=builder /usr/bin/caddy /usr/bin/caddy
