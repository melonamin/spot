# Optional Caddy image with the Cloudflare DNS module, needed for DNS-01
# wildcard certificates when SPOT_TLS_MODE=tls-cloudflare.
# Cross-compile on the build platform; xcaddy honors GOOS/GOARCH, so multi-arch
# builds avoid QEMU emulation. buildx populates BUILDPLATFORM/TARGETOS/TARGETARCH.
FROM --platform=$BUILDPLATFORM caddy:2.11.4-builder AS builder
ARG TARGETOS TARGETARCH
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH xcaddy build --with github.com/caddy-dns/cloudflare@v0.2.4

FROM caddy:2.11.4-alpine
COPY --from=builder /usr/bin/caddy /usr/bin/caddy
