#!/usr/bin/env bash
# Install RHIS Remote on Debian 13 (trixie).
#
#   sudo ./install.sh                         # latest production release on https://<server-ip> (internal certificate)
#   sudo ./install.sh --domain remote.rhis.nl # HTTPS with a free Let's Encrypt certificate
#   sudo ./install.sh --channel test          # latest test pre-release
#   sudo ./install.sh --domain remote.rhis.nl --behind-proxy 10.10.4.10
#                                             # behind a reverse proxy (Zoraxy, nginx, ...) that
#                                             # owns the public certificate for that domain
#   sudo ./install.sh --binary ./rhis-remote  # install a binary you built yourself
#   sudo ./install.sh --public-ip 203.0.113.4 # public address of the built-in TURN relay (behind NAT)
#
# A separate relay server (no database, no portal) joins an existing control server with a
# one-time token from Settings > Servers in the portal:
#
#   sudo ./install.sh --role relay --control https://remote.rhis.nl --token rj_... [--relay-domain relay1.rhis.nl]
#
# All traffic is encrypted: Caddy terminates HTTPS on port 443 and the server itself only
# listens on 127.0.0.1. Without --domain, Caddy's own internal CA issues the certificate;
# the installer prints its fingerprint so you can check it or trust it in your browser.
#
# Behind a reverse proxy, the proxy terminates the public HTTPS and forwards to
# https://<server-ip>:443 here, so the hop to this server stays encrypted too. Caddy only
# passes on X-Forwarded-For only from the proxy address(es) given with --behind-proxy.
#
# Running it again upgrades the binary and keeps the configuration, database and the
# --domain/--behind-proxy choice (stored in /etc/rhis-remote/install.env).
set -euo pipefail

REPO="rhisijmuiden/remote-support-tool"
ASSET="rhis-remote_linux_amd64.tar.gz"
CHANNEL="production"
DOMAIN=""
PROXY=""
BINARY=""
ROLE="all"
CONTROL=""
TOKEN=""
RELAY_DOMAIN=""
PUBLIC_IP=""
# Choices from an earlier run; command-line options override them.
if [[ -f /etc/rhis-remote/install.env ]]; then
  # shellcheck disable=SC1091
  . /etc/rhis-remote/install.env
  DOMAIN="${RHIS_DOMAIN:-}"; PROXY="${RHIS_PROXY:-}"
fi
while [[ $# -gt 0 ]]; do
  case "$1" in
    --domain) DOMAIN="$2"; shift 2 ;;
    --behind-proxy) PROXY="$2"; shift 2 ;;
    --direct) PROXY=""; shift ;;
    --role) ROLE="$2"; shift 2 ;;
    --control) CONTROL="${2%/}"; shift 2 ;;
    --token) TOKEN="$2"; shift 2 ;;
    --relay-domain) RELAY_DOMAIN="$2"; shift 2 ;;
    --public-ip) PUBLIC_IP="$2"; shift 2 ;;
    --channel) CHANNEL="$2"; shift 2 ;;
    --binary) BINARY="$(realpath "$2")"; shift 2 ;;
    -h|--help) sed -n '2,20p' "$0"; exit 0 ;;
    *) echo "Unknown option: $1" >&2; exit 2 ;;
  esac
done
[[ $EUID -eq 0 ]] || { echo "Run as root (sudo)." >&2; exit 1; }
[[ "$CHANNEL" == "production" || "$CHANNEL" == "test" ]] || { echo "--channel must be production or test" >&2; exit 2; }
[[ "$ROLE" == "all" || "$ROLE" == "relay" ]] || { echo "--role must be all or relay" >&2; exit 2; }
if [[ "$ROLE" == "relay" ]]; then
  [[ "$CONTROL" == https://* ]] || { echo "--control must be the https:// address of the control server" >&2; exit 2; }
  [[ -n "$TOKEN" || -f /etc/rhis-remote/config.yaml ]] || { echo "--token is required (Settings > Servers > Add relay server)" >&2; exit 2; }
fi
if [[ -n "$PROXY" ]]; then
  [[ -n "$DOMAIN" ]] || { echo "--behind-proxy needs --domain: the public name the proxy serves." >&2; exit 2; }
  # Comma or space separated IPs or CIDR ranges of the proxy.
  PROXY="$(echo "$PROXY" | tr ',' ' ' | xargs)"
  for p in $PROXY; do
    [[ "$p" =~ ^[0-9a-fA-F:.]+(/[0-9]{1,3})?$ ]] || { echo "--behind-proxy: '$p' is not an IP address or CIDR range" >&2; exit 2; }
  done
fi
[[ -z "$DOMAIN" || "$DOMAIN" =~ ^[A-Za-z0-9.-]+$ ]] || { echo "--domain: '$DOMAIN' is not a host name" >&2; exit 2; }

say() { printf '\n\033[1;38;2;255;107;87m==>\033[0m %s\n' "$*"; }

say "Installing packages"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
# postgresql-common first, so its debconf script finds pg_lsclusters
if [[ "$ROLE" == "relay" ]]; then
  # A relay needs no database and no web server.
  apt-get install -y -qq ca-certificates curl jq openssl >/dev/null
else
  apt-get install -y -qq postgresql-common >/dev/null
  apt-get install -y -qq postgresql postgresql-client ca-certificates curl jq openssl caddy >/dev/null
fi

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
  # Windows clients (agent, quick support) that the portal offers for download.
  if [[ -d "$tmp/agent" ]]; then
    for f in "$tmp"/agent/*; do install -o rhis-remote -g rhis-remote -m 0644 "$f" /opt/rhis-remote/agent/; done
  fi
  # Use the service file from the same release as the binary.
  SERVICE_FILE="$tmp/rhis-remote.service"
fi
/opt/rhis-remote/bin/rhis-remote version

if [[ "$ROLE" == "relay" && ! -f /etc/rhis-remote/config.yaml ]]; then
  say "Creating the relay configuration"
  tls_listen='":5349"'
  [[ -n "$RELAY_DOMAIN" ]] && tls_listen='":443"'
  cat > /etc/rhis-remote/config.yaml <<CFG
# RHIS Remote relay server. It registers with the control server over HTTPS and needs
# no database. Its name and roles are managed in the portal (Settings > Servers).
roles: ["relay"]
data_dir: "/var/lib/rhis-remote/data"
node:
  name: "$(hostname)"
join:
  control_url: "$CONTROL"
  # One-time token: exchanged for a credential (stored in data_dir/node.json) on first start.
  token: "$TOKEN"
relay:
  listen: ":3478"                 # TURN over UDP and TCP
  tls_listen: $tls_listen
  domain: "$RELAY_DOMAIN"         # with a domain: Let's Encrypt certificate and TURNS on 443
  public_ip: "$PUBLIC_IP"         # empty: the address the control server sees
  min_port: 49160
  max_port: 49359
update:
  repo: "$REPO"
  asset: "$ASSET"
CFG
  chown root:rhis-remote /etc/rhis-remote/config.yaml
  chmod 0640 /etc/rhis-remote/config.yaml
elif [[ "$ROLE" == "relay" ]]; then
  :
elif [[ ! -f /etc/rhis-remote/config.yaml ]]; then
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
# Built-in TURN relay. Forward 3478 udp+tcp and udp 49160-49359 straight to this server
# (not through an HTTP reverse proxy). public_ip: the address customers reach it on.
relay:
  public_ip: "$PUBLIC_IP"
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
if [[ -n "$DOMAIN" ]] && ! grep -q "^public_url: \"https://$DOMAIN\"" /etc/rhis-remote/config.yaml; then
  say "Setting the public address to https://$DOMAIN"
  sed -i -E "s|^public_url: .*|public_url: \"https://$DOMAIN\"|" /etc/rhis-remote/config.yaml
fi
# Tell the server which proxy hops to skip when it looks for the visitor's address.
sed -i '/^trusted_proxies:/d' /etc/rhis-remote/config.yaml
if [[ -n "$PROXY" ]]; then
  echo "trusted_proxies: [$(printf '"%s",' $PROXY | sed 's/,$//')]" >> /etc/rhis-remote/config.yaml
fi
cat > /etc/rhis-remote/install.env <<ENV
# Written by install.sh; used when it runs again.
RHIS_DOMAIN="$DOMAIN"
RHIS_PROXY="$PROXY"
ENV
chmod 0640 /etc/rhis-remote/install.env

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

if [[ "$ROLE" == "relay" ]]; then
  for i in $(seq 1 20); do
    curl -fsS http://127.0.0.1:8080/healthz >/dev/null 2>&1 && break
    sleep 1
  done
  if curl -fsS http://127.0.0.1:8080/healthz >/dev/null 2>&1; then
    # The token is single use; it has been exchanged for a credential by now.
    sed -i -E 's|^(  token: ).*|\1""                    # used; the credential is in data_dir/node.json|' /etc/rhis-remote/config.yaml
    say "Relay is running and registered with $CONTROL. It appears under Settings > Servers."
    echo "Open these ports to this server: 3478 udp+tcp, 49160-49359 udp$([[ -n "$RELAY_DOMAIN" ]] && echo ', 443 tcp')."
    exit 0
  fi
  echo "The relay did not come up. Check: journalctl -u rhis-remote -n 50" >&2
  exit 1
fi

say "Setting up HTTPS with Caddy"
ip="$(hostname -I | awk '{print $1}')"
if [[ -n "$PROXY" ]]; then
  cat > /etc/caddy/Caddyfile.new <<CADDY
# Managed by the RHIS Remote installer. Behind the reverse proxy $PROXY, which serves
# https://$DOMAIN publicly. Caddy keeps the hop from the proxy encrypted with its internal CA.
# Only requests from the proxy may pass on their X-Forwarded-For; for anyone else Caddy
# reports the TCP peer itself. Works with Debian's Caddy 2.6 (no trusted_proxies needed).
{
	default_sni $ip
}
https://$DOMAIN, https://$ip, https://$(hostname) {
	tls internal
	encode zstd gzip
	@proxy remote_ip $PROXY
	reverse_proxy @proxy 127.0.0.1:8080 {
		header_up X-Forwarded-For "{http.request.header.X-Forwarded-For}, {http.request.remote.host}"
	}
	reverse_proxy 127.0.0.1:8080 {
		header_up X-Forwarded-For {http.request.remote.host}
		header_up -X-Real-IP
	}
}
CADDY
elif [[ -n "$DOMAIN" ]]; then
  cat > /etc/caddy/Caddyfile.new <<CADDY
# Managed by the RHIS Remote installer. HTTPS with Let's Encrypt for $DOMAIN.
$DOMAIN {
	encode zstd gzip
	reverse_proxy 127.0.0.1:8080
}
CADDY
else
  cat > /etc/caddy/Caddyfile.new <<CADDY
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
caddy validate --config /etc/caddy/Caddyfile.new --adapter caddyfile >/dev/null 2>&1 \
  || { caddy validate --config /etc/caddy/Caddyfile.new --adapter caddyfile; echo "Generated Caddyfile is invalid; kept the old one." >&2; exit 1; }
mv /etc/caddy/Caddyfile.new /etc/caddy/Caddyfile
systemctl enable --now caddy >/dev/null
systemctl reload caddy || systemctl restart caddy

for i in $(seq 1 30); do
  curl -fsS http://127.0.0.1:8080/healthz >/dev/null 2>&1 && break
  sleep 1
done
if curl -fsS http://127.0.0.1:8080/healthz >/dev/null 2>&1; then
  url="$(grep '^public_url:' /etc/rhis-remote/config.yaml | cut -d'"' -f2)"
  if [[ -z "$DOMAIN" || -n "$PROXY" ]]; then
    curl -sk -o /dev/null "https://$ip/" || true
    root=/var/lib/caddy/.local/share/caddy/pki/authorities/local/root.crt
    for i in $(seq 1 10); do [[ -f "$root" ]] && break; sleep 1; done
    if [[ -f "$root" ]]; then
      echo
      echo "The certificate comes from this server's own CA. Your browser warns until you trust it."
      echo "Root CA: $root"
      echo "SHA-256 fingerprint: $(openssl x509 -in "$root" -noout -fingerprint -sha256 | cut -d= -f2)"
      if [[ -n "$PROXY" ]]; then
        echo "In your reverse proxy, forward https://$DOMAIN to https://$ip:443 and either import"
        echo "that root CA as trusted upstream CA or enable 'skip TLS verification' for this upstream."
        echo "Keep the original Host header and enable WebSocket support."
      else
        echo "Import that file as a trusted root on your PC, or use --domain with a public DNS name."
      fi
    fi
  fi
  say "RHIS Remote is running. Open $url to create the first administrator."
else
  echo "The service did not come up. Check: journalctl -u rhis-remote -n 50" >&2
  exit 1
fi
