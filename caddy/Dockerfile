# Optional Caddy image with the Cloudflare DNS module, needed for DNS-01
# wildcard certificates when SPOT_TLS_MODE=tls-cloudflare.
FROM caddy:2.11.4-builder AS builder
RUN xcaddy build --with github.com/caddy-dns/cloudflare@v0.2.4

FROM caddy:2.11.4-alpine
COPY --from=builder /usr/bin/caddy /usr/bin/caddy
