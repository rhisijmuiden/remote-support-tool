#!/usr/bin/env bash
# Install RHIS Remote on Debian 13 (trixie).
#
#   sudo ./install.sh                         # latest production release on https://<server-ip> (internal certificate)
#   sudo ./install.sh --domain remote.rhis.nl # HTTPS with a free Let's Encrypt certificate
#   sudo ./install.sh --channel test          # latest test pre-release
#   sudo ./install.sh --binary ./rhis-remote  # install a binary you built yourself
#
# All traffic is encrypted: Caddy terminates HTTPS on port 443 and the server itself only
# listens on 127.0.0.1. Without --domain, Caddy's own internal CA issues the certificate;
# the installer prints its fingerprint so you can check it or trust it in your browser.
#
# Running it again upgrades the binary and keeps the configuration and database.
set -euo pipefail

REPO="rhisijmuiden/remote-support-tool"
ASSET="rhis-remote_linux_amd64.tar.gz"
CHANNEL="production"
DOMAIN=""
BINARY=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --domain) DOMAIN="$2"; shift 2 ;;
    --channel) CHANNEL="$2"; shift 2 ;;
    --binary) BINARY="$(realpath "$2")"; shift 2 ;;
    -h|--help) sed -n '2,10p' "$0"; exit 0 ;;
    *) echo "Unknown option: $1" >&2; exit 2 ;;
  esac
done
[[ $EUID -eq 0 ]] || { echo "Run as root (sudo)." >&2; exit 1; }
[[ "$CHANNEL" == "production" || "$CHANNEL" == "test" ]] || { echo "--channel must be production or test" >&2; exit 2; }

say() { printf '\n\033[1;38;2;255;107;87m==>\033[0m %s\n' "$*"; }

say "Installing packages"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
# postgresql-common first, so its debconf script finds pg_lsclusters
apt-get install -y -qq postgresql-common >/dev/null
apt-get install -y -qq postgresql postgresql-client ca-certificates curl jq openssl caddy >/dev/null

say "Creating user and folders"
id rhis-remote >/dev/null 2>&1 || useradd --system --home /var/lib/rhis-remote --shell /usr/sbin/nologin rhis-remote
install -d -o rhis-remote -g rhis-remote -m 0750 /var/lib/rhis-remote /var/lib/rhis-remote/data
# The service owns its binary folder so it can install updates from the portal.
install -d -o rhis-remote -g rhis-remote -m 0755 /opt/rhis-remote /opt/rhis-remote/bin
install -d -o root -g rhis-remote -m 0750 /etc/rhis-remote

say "Installing the server binary"
if [[ -n "$BINARY" ]]; then
  install -o rhis-remote -g rhis-remote -m 0755 "$BINARY" /opt/rhis-remote/bin/rhis-remote
else
  tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
  SERVICE_FILE=""
  filter='[.[] | select(.draft|not)]'
  [[ "$CHANNEL" == "production" ]] && filter='[.[] | select((.draft|not) and (.prerelease|not))]'
  tag="$(curl -fsSL "https://api.github.com/repos/$REPO/releases?per_page=50" | jq -r "$filter | .[0].tag_name // empty")"
  [[ -n "$tag" ]] || { echo "No $CHANNEL release found for $REPO. Use --binary or --channel test." >&2; exit 1; }
  echo "Release $tag"
  base="https://github.com/$REPO/releases/download/$tag"
  curl -fsSL -o "$tmp/$ASSET" "$base/$ASSET"
  curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt"
  (cd "$tmp" && grep " $ASSET\$" checksums.txt | sha256sum -c -)
  tar -xzf "$tmp/$ASSET" -C "$tmp"
  install -o rhis-remote -g rhis-remote -m 0755 "$tmp/rhis-remote" /opt/rhis-remote/bin/rhis-remote
  # Use the service file from the same release as the binary.
  SERVICE_FILE="$tmp/rhis-remote.service"
fi
/opt/rhis-remote/bin/rhis-remote version

if [[ ! -f /etc/rhis-remote/config.yaml ]]; then
  say "Creating database and configuration"
  dbpass="$(openssl rand -hex 24)"
  if ! runuser -u postgres -- psql -tAc "SELECT 1 FROM pg_roles WHERE rolname='rhis_remote'" | grep -q 1; then
    runuser -u postgres -- psql -q -c "CREATE ROLE rhis_remote LOGIN PASSWORD '$dbpass'"
  else
    runuser -u postgres -- psql -q -c "ALTER ROLE rhis_remote PASSWORD '$dbpass'"
  fi
  runuser -u postgres -- psql -tAc "SELECT 1 FROM pg_database WHERE datname='rhis_remote'" | grep -q 1 \
    || runuser -u postgres -- createdb -O rhis_remote rhis_remote
  if [[ -n "$DOMAIN" ]]; then url="https://$DOMAIN"; else url="https://$(hostname -I | awk '{print $1}')"; fi
  cat > /etc/rhis-remote/config.yaml <<CFG
# Only reachable through Caddy (HTTPS). Never expose this port directly.
listen: "127.0.0.1:8080"
public_url: "$url"
database_url: "postgres://rhis_remote:$dbpass@localhost/rhis_remote?sslmode=disable"
data_dir: "/var/lib/rhis-remote/data"
# Never change secret_key: it decrypts the stored mail and backup passwords.
secret_key: "$(/opt/rhis-remote/bin/rhis-remote gen-secret)"
roles: ["all"]
update:
  repo: "$REPO"
  asset: "$ASSET"
branding:
  product_name: "RHIS Remote"
  company_name: "RHIS"
CFG
  chown root:rhis-remote /etc/rhis-remote/config.yaml
  chmod 0640 /etc/rhis-remote/config.yaml
elif grep -qE '^listen: "(:|0\.0\.0\.0:)8080"' /etc/rhis-remote/config.yaml && ! grep -q '^allow_insecure_http: true' /etc/rhis-remote/config.yaml; then
  say "Moving the existing installation behind HTTPS"
  sed -i -E 's|^listen: .*|listen: "127.0.0.1:8080"|' /etc/rhis-remote/config.yaml
  if [[ -n "$DOMAIN" ]]; then url="https://$DOMAIN"; else url="https://$(hostname -I | awk '{print $1}')"; fi
  sed -i -E "s|^public_url: .*|public_url: \"$url\"|" /etc/rhis-remote/config.yaml
fi

say "Installing the service"
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [[ -z "${SERVICE_FILE:-}" || ! -f "$SERVICE_FILE" ]]; then
  SERVICE_FILE="$script_dir/rhis-remote.service"
fi
[[ -f "$SERVICE_FILE" ]] || { echo "rhis-remote.service not found next to install.sh or in the release." >&2; exit 1; }
install -m 0644 "$SERVICE_FILE" /etc/systemd/system/rhis-remote.service
systemctl daemon-reload
systemctl enable --now rhis-remote >/dev/null
systemctl restart rhis-remote

say "Setting up HTTPS with Caddy"
ip="$(hostname -I | awk '{print $1}')"
if [[ -n "$DOMAIN" ]]; then
  cat > /etc/caddy/Caddyfile <<CADDY
# Managed by the RHIS Remote installer. HTTPS with Let's Encrypt for $DOMAIN.
$DOMAIN {
	encode zstd gzip
	reverse_proxy 127.0.0.1:8080
}
CADDY
else
  cat > /etc/caddy/Caddyfile <<CADDY
# Managed by the RHIS Remote installer. HTTPS with Caddy's internal CA (no public domain).
{
	default_sni $ip
}
https://$ip, https://$(hostname) {
	tls internal
	encode zstd gzip
	reverse_proxy 127.0.0.1:8080
}
CADDY
fi
systemctl enable --now caddy >/dev/null
systemctl reload caddy || systemctl restart caddy

for i in $(seq 1 30); do
  curl -fsS http://127.0.0.1:8080/healthz >/dev/null 2>&1 && break
  sleep 1
done
if curl -fsS http://127.0.0.1:8080/healthz >/dev/null 2>&1; then
  url="$(grep '^public_url:' /etc/rhis-remote/config.yaml | cut -d'"' -f2)"
  if [[ -z "$DOMAIN" ]]; then
    curl -sk -o /dev/null "https://$ip/" || true
    root=/var/lib/caddy/.local/share/caddy/pki/authorities/local/root.crt
    for i in $(seq 1 10); do [[ -f "$root" ]] && break; sleep 1; done
    if [[ -f "$root" ]]; then
      echo
      echo "The certificate comes from this server's own CA. Your browser warns until you trust it."
      echo "Root CA: $root"
      echo "SHA-256 fingerprint: $(openssl x509 -in "$root" -noout -fingerprint -sha256 | cut -d= -f2)"
      echo "Import that file as a trusted root on your PC, or use --domain with a public DNS name."
    fi
  fi
  say "RHIS Remote is running. Open $url to create the first administrator."
else
  echo "The service did not come up. Check: journalctl -u rhis-remote -n 50" >&2
  exit 1
fi
