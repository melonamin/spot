# Caddy with the Cloudflare DNS module, needed for the DNS-01 wildcard
# challenge (SPOT_TLS_MODE=tls-cloudflare). Built by the mesh overlay;
# plain local dev keeps the stock caddy:2-alpine image.
FROM caddy:2-builder AS builder
RUN xcaddy build --with github.com/caddy-dns/cloudflare

FROM caddy:2-alpine
COPY --from=builder /usr/bin/caddy /usr/bin/caddy
