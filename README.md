# RHIS Remote

Self-hosted remote support for Windows: remote control with unattended access, a one-off quick support client, and a modular server that runs on a single Debian 13 machine and scales out to multiple servers.

> **Status:** early development. Version 0.1 contains the server foundation and the admin portal. The Windows agent, quick support client and client builder come next, see the [roadmap](ROADMAP.md).

![Dashboard](docs/screenshots/dashboard.png)

## What is in the server today

- **Admin portal** in the RHIS house style, English by default with a full Dutch translation, light and dark theme, menu on the left.
- **Users and roles:** administrators and technicians, Argon2id password hashing, login rate limiting, activity log.
- **Mail server:** your own SMTP server with STARTTLS or implicit TLS (minimum TLS 1.2 or 1.3), authentication by SCRAM-SHA-256, PLAIN, LOGIN, CRAM-MD5 or XOAUTH2, and a test email from the portal.
- **Backup and restore:** full and incremental backups of the database and data folder to FTP, FTPS (explicit or implicit) or SCP over SSH, on a schedule or by hand, optionally encrypted with [age](https://age-encryption.org). Retention keeps the newest N full backups. Restore works from the remote location, also on a fresh server.
- **Info and updates:** the Info button shows the version and checks GitHub releases. Switch between the production and test channel and install an update from the portal.
- **Session audit and recording:** every remote session is logged (technician, device, customer, time, duration, connection type, IP) with a timeline of what happened, filterable and exportable as CSV. Optional video recording (off, technician chooses, or always), stored encrypted on the server, with playback, download, retention and private share links for customers. See [docs/session-recording.md](docs/session-recording.md).
- **Client builder:** branded installers and QuickSupport clients per customer, with your logo, colour, product name and an ordered list of `https://` servers. The customer's enrolment token is baked in; send the installer with an expiring download link, revoke a client or replace its token at any time. See [docs/client-builder.md](docs/client-builder.md).
- **Central agent updates:** the server imports agent releases from GitHub (or an upload) and keeps every agent up to date, per channel, with pinned versions, per-customer exceptions and a staged rollout. Update manifests are signed with the server's own Ed25519 key, which every client carries.
- **Code signing:** installers and agent updates are Authenticode-signed on the server, with an uploaded PFX certificate or a signing command for keys on a hardware token, HSM or cloud service.
- **One binary, several roles, scales out:** `roles: ["all"]` on one machine, including a TURN relay for connections behind strict NAT. Add relay servers close to your customers with a one-time token from the portal (no database needed, they update themselves), or run several control servers on a shared PostgreSQL with NATS. All traffic between servers is encrypted. See [docs/scaling.md](docs/scaling.md).

## Screenshots

All screenshots are taken from a real `v0.1.0-test.1` server. The backups shown were made to an FTPS server.

| | |
| --- | --- |
| ![Sign in](docs/screenshots/login.png) **Sign in** | ![Dark theme in Dutch](docs/screenshots/dashboard-dark-nl.png) **Dark theme, Dutch** |
| ![Users and roles](docs/screenshots/users.png) **Users and roles** | ![Mail server](docs/screenshots/mail.png) **Mail server (SMTP)** |
| ![Backup and restore](docs/screenshots/backup.png) **Backup history and restore** | ![Info and updates](docs/screenshots/info-updates.png) **Info, update channel and updates** |
| ![Activity log](docs/screenshots/audit.png) **Activity log** | ![Session recording settings](docs/screenshots/recording-settings.png) **Session recording settings** |
| ![Sessions](docs/screenshots/sessions.png) **Session log** | ![Session with recording](docs/screenshots/session-detail.png) **Session details, recording and timeline** |
| ![Share link](docs/screenshots/session-share.png) **Share a recording with a customer** | ![Customer share page](docs/screenshots/shared-recording-nl.png) **What the customer sees (Dutch)** |
| ![Sessions in Dutch, dark theme](docs/screenshots/sessions-nl-dark.png) **Sessions, dark theme, Dutch** | ![Servers](docs/screenshots/servers.png) **Servers: control server and relays** |
| ![Add a relay server](docs/screenshots/servers-add.png) **Add a relay server with a one-time token** | ![Servers in Dutch, dark theme](docs/screenshots/servers-nl-dark.png) **Servers, dark theme, Dutch** |
| ![Client builder](docs/screenshots/client-builder.png) **Client builder** | ![Client details](docs/screenshots/client-builder-detail.png) **Client: branding, servers, installers and download links** |
| ![Edit a client](docs/screenshots/client-builder-edit.png) **Logo, colour, type and server failover list** | ![Agent updates](docs/screenshots/agent-updates.png) **Agent updates: policy, staged rollout, releases** |
| ![Code signing](docs/screenshots/code-signing.png) **Code signing** | ![Client in Dutch, dark theme](docs/screenshots/client-builder-detail-nl-dark.png) **Signed installer, dark theme, Dutch** |

The Servers screenshots show a real control server with a relay that joined with a token, plus one demo relay; the session screenshots use demo data; the recording in them was made by a real browser with the portal's recorder and played back from the encrypted store.

## Roadmap

| Phase | Status |
| --- | --- |
| 1. Server foundation | Done (v0.1) |
| 2. Windows agent and connectivity (NAT traversal, WebRTC, relay) | Next |
| 3. Quick support client (no install, session code) | Planned |
| 4. Session audit and recording | Server and portal in test; recording goes live with the Windows agent |
| 5. Client builder and signed central updates | Server and portal in test; the agent side of updates comes with the Windows agent |
| 6. Multi-server and scale-out (relays, NATS cluster) | Relays, cluster and Servers page in test |
| 7. Code signing and open-source release | Planned |

Details per phase are in [ROADMAP.md](ROADMAP.md) and in the [roadmap issues](https://github.com/rhisijmuiden/remote-support-tool/issues?q=label%3Aroadmap).

## Install on Debian 13

```bash
curl -fsSLO https://raw.githubusercontent.com/rhisijmuiden/remote-support-tool/main/deploy/debian/install.sh
sudo bash install.sh --domain remote.example.com   # HTTPS with a Let's Encrypt certificate
# or: sudo bash install.sh                         # https://<server-ip> with an internal certificate
# or: sudo bash install.sh --channel test          # newest test pre-release
# add --public-ip <address> when the server is behind NAT, for the built-in TURN relay
```

A separate relay server is installed with the command from **Settings > Servers > Add relay server** (`install.sh --role relay ...`). Forward 3478 UDP+TCP and 49160-49359 UDP straight to the server running the relay, past any HTTP reverse proxy such as Zoraxy. See [docs/scaling.md](docs/scaling.md).

The installer sets up PostgreSQL, Caddy, a `rhis-remote` system user, `/etc/rhis-remote/config.yaml`, and a hardened systemd service. Open the URL it prints and create the first administrator.

**All traffic is encrypted.** Caddy serves HTTPS on port 443 (HTTP only redirects) and the server itself listens on `127.0.0.1` only. Without `--domain`, Caddy's internal CA issues the certificate; the installer prints the root CA path and SHA-256 fingerprint so you can verify it and trust it on your PCs. The server refuses to start with a plain-HTTP `public_url` or a non-loopback listen address without TLS, unless `allow_insecure_http: true` is set for a lab.

### Behind a reverse proxy (Zoraxy, nginx, Traefik, ...)

If another reverse proxy already owns ports 80/443 and the public certificate, install with the public name and the proxy's IP address (or CIDR range):

```bash
sudo bash install.sh --domain remote.example.com --behind-proxy 10.10.4.10
```

Then point the proxy at **`https://<server-ip>:443`**, so the hop between the proxy and this server is encrypted as well. In the proxy:

- Forward `remote.example.com` to `https://<server-ip>:443` and keep the original `Host` header.
- The upstream certificate comes from Caddy's internal CA. Import the root CA the installer prints as a trusted upstream CA, or enable *skip TLS verification* for this upstream (in Zoraxy: the upstream's "Skip TLS validation" option).
- Enable WebSocket support (needed for live sessions).
- Make sure the proxy sends `X-Forwarded-For` and `X-Forwarded-Proto` (Zoraxy and nginx do this by default or with the usual `proxy_set_header` lines).

The server only believes forwarding headers from trusted hops: the local Caddy and the addresses in `trusted_proxies` (written by `--behind-proxy`). Login rate limiting and the activity log therefore see the real visitor, and a client cannot spoof its address. The installer remembers `--domain` and `--behind-proxy` in `/etc/rhis-remote/install.env`; use `--direct` to drop the proxy setup.

## Branches and releases

| Branch | Channel | Release |
| --- | --- | --- |
| `main` | Production | `vX.Y.Z` from the `VERSION` file |
| `test` | Test | `vX.Y.Z-test.N` pre-release on every push |

Feature work goes into `test` first. The Info screen in the portal switches a server between the two channels.

## Development

Requirements: Go 1.26, Node 22, PostgreSQL 17 (with `pg_dump`/`pg_restore` on the path).

```bash
make web                       # build the portal into the Go binary
make server                    # bin/rhis-remote
RHIS_REMOTE_DATABASE_URL=postgres://localhost/rhis_dev?sslmode=disable \
RHIS_REMOTE_SECRET_KEY=$(./bin/rhis-remote gen-secret) \
RHIS_REMOTE_DATA_DIR=./data ./bin/rhis-remote serve --config config.local.yaml   # http://127.0.0.1:8080
cd web && npm run dev          # portal with hot reload on :5173, API proxied to :8080
RHIS_TEST_DATABASE_URL=postgres://localhost/rhis_test?sslmode=disable make test
```

| Folder | Contents |
| --- | --- |
| `server/` | Go server (`cmd/rhis-remote`, packages in `internal/`) |
| `web/` | React + TypeScript portal (Vite, i18next) |
| `deploy/debian/` | Installer, systemd unit, example config |
| `branding/` | RHIS house style: logos, colors, fonts |
| `docs/screenshots/` | Portal screenshots used in this README |

## Branding

The `branding/` folder holds the RHIS house style used by the portal. Product and company name are set in `config.yaml` under `branding`, so other builds can ship their own name.

## License

Copyright (C) 2026 RHIS.

This project is licensed under the [GNU Affero General Public License v3.0](LICENSE) (AGPL-3.0-only). If you run a modified version as a network service, you must make your source code available to its users. The portal links to this repository from its Info screen.
