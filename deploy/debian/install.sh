#!/usr/bin/env bash
# Install RHIS Remote on Debian 13 (trixie).
#
#   sudo ./install.sh                         # latest production release, reachable on http://<ip>:8080
#   sudo ./install.sh --domain remote.rhis.nl # also installs Caddy for HTTPS with Let's Encrypt
#   sudo ./install.sh --channel test          # latest test pre-release
#   sudo ./install.sh --binary ./rhis-remote  # install a binary you built yourself
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
apt-get install -y -qq postgresql postgresql-client ca-certificates curl jq openssl >/dev/null

say "Creating user and folders"
id rhis-remote >/dev/null 2>&1 || useradd --system --home /var/lib/rhis-remote --shell /usr/sbin/nologin rhis-remote
install -d -o rhis-remote -g rhis-remote -m 0750 /var/lib/rhis-remote /var/lib/rhis-remote/data
# The service owns its binary folder so it can install updates from the portal.
install -d -o rhis-remote -g rhis-remote -m 0755 /opt/rhis-remote /opt/rhis-remote/bin /opt/rhis-remote/agent
install -d -o root -g rhis-remote -m 0750 /etc/rhis-remote

say "Installing the server binary"
if [[ -n "$BINARY" ]]; then
  install -o rhis-remote -g rhis-remote -m 0755 "$BINARY" /opt/rhis-remote/bin/rhis-remote
else
  tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
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
  # Windows clients (agent, quick support) that the portal offers for download.
  if [[ -d "$tmp/agent" ]]; then
    for f in "$tmp"/agent/*; do install -o rhis-remote -g rhis-remote -m 0644 "$f" /opt/rhis-remote/agent/; done
  fi
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
  if [[ -n "$DOMAIN" ]]; then listen="127.0.0.1:8080"; url="https://$DOMAIN"; else listen=":8080"; url="http://$(hostname -I | awk '{print $1}'):8080"; fi
  cat > /etc/rhis-remote/config.yaml <<CFG
listen: "$listen"
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
fi

say "Installing the service"
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [[ -f "$script_dir/rhis-remote.service" ]]; then
  install -m 0644 "$script_dir/rhis-remote.service" /etc/systemd/system/rhis-remote.service
else
  curl -fsSL -o /etc/systemd/system/rhis-remote.service "https://raw.githubusercontent.com/$REPO/main/deploy/debian/rhis-remote.service"
fi
systemctl daemon-reload
systemctl enable --now rhis-remote >/dev/null
systemctl restart rhis-remote

if [[ -n "$DOMAIN" ]]; then
  say "Setting up HTTPS with Caddy for $DOMAIN"
  apt-get install -y -qq caddy >/dev/null
  cat > /etc/caddy/Caddyfile <<CADDY
$DOMAIN {
	encode zstd gzip
	reverse_proxy 127.0.0.1:8080
}
CADDY
  systemctl reload caddy || systemctl restart caddy
fi

for i in $(seq 1 30); do
  curl -fsS http://127.0.0.1:8080/healthz >/dev/null 2>&1 && break
  sleep 1
done
if curl -fsS http://127.0.0.1:8080/healthz >/dev/null 2>&1; then
  url="$(grep '^public_url:' /etc/rhis-remote/config.yaml | cut -d'"' -f2)"
  say "RHIS Remote is running. Open $url to create the first administrator."
else
  echo "The service did not come up. Check: journalctl -u rhis-remote -n 50" >&2
  exit 1
fi
