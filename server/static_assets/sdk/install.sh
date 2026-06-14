#!/bin/sh
# Install the Spot CLI from this Spot server.
set -eu

spot_url=${1:-${SPOT_URL:-}}
if [ -z "$spot_url" ]; then
    echo "usage: curl -fsSL https://spot.example.com/install.sh | sh -s -- https://spot.example.com" >&2
    exit 1
fi

case $spot_url in
    http://*|https://*) ;;
    *)
        echo "error: Spot URL must start with http:// or https://" >&2
        exit 1
        ;;
esac

spot_url=${spot_url%/}
install_dir=${SPOT_INSTALL_DIR:-"$HOME/.local/bin"}
config_home=${XDG_CONFIG_HOME:-"$HOME/.config"}
config_dir="$config_home/spot"
tmp=${TMPDIR:-/tmp}/spot-cli.$$
curl_opts="-fsSL"

# Local HTTPS dev may use a private CA; only a true localhost-suffixed
# hostname (anchored match, like the CLI's) skips verification.
host=${spot_url#*://}
host=${host%%/*}
host=${host%%:*}
case $host in
    localhost|*.localhost) curl_opts="$curl_opts -k" ;;
esac

cleanup() {
    rm -f "$tmp"
}
trap cleanup EXIT INT TERM

mkdir -p "$install_dir" "$config_dir"
curl $curl_opts "$spot_url/spot" -o "$tmp"
install -m 0755 "$tmp" "$install_dir/spot"
printf 'SPOT_URL=%s\n' "$spot_url" > "$config_dir/env"

echo "installed spot -> $install_dir/spot"
echo "configured SPOT_URL=$spot_url -> $config_dir/env"

case ":$PATH:" in
    *":$install_dir:"*) ;;
    *)
        echo "warning: $install_dir is not on PATH" >&2
        echo "add this to your shell profile: export PATH=\"\$HOME/.local/bin:\$PATH\"" >&2
        ;;
esac
