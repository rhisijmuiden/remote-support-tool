# Roadmap

RHIS Remote is built in phases. Each phase ships as test pre-releases (`vX.Y.Z-test.N` from the `test` branch) first and is promoted to a production release (`vX.Y.Z` from `main`) once it has been tested.

Every phase has a tracking issue labelled `roadmap` on GitHub. Detailed tasks are added to that issue when the phase starts.

| Phase | Status | Target version | Issue |
| --- | --- | --- | --- |
| 1. Server foundation | Done | v0.1 | #4 |
| 2. Windows agent and connectivity | Next | v0.2 | #5 |
| 3. Quick support client | Planned | v0.3 | #6 |
| 4. Session audit and recording | In progress | v0.4 | #7 |
| 5. Client builder and central updates | Planned | v0.5 | #8 |
| 6. Multi-server and scale-out | In progress | v0.6 | #9 |
| 7. Code signing and open-source release | Planned | v1.0 | #10 |

## 1. Server foundation (done)

Released as `v0.1.0-test.1`.

- [x] One Go binary with roles, PostgreSQL storage, Debian 13 installer with systemd and Caddy (HTTPS via Let's Encrypt)
- [x] React + TypeScript admin portal in the RHIS house style: menu on the left, light and dark theme, English with a full Dutch translation
- [x] Users and roles (administrator, technician), Argon2id passwords, login rate limiting, activity log
- [x] Configurable SMTP mail server: STARTTLS or implicit TLS, TLS 1.2/1.3 minimum, SCRAM-SHA-256, PLAIN, LOGIN, CRAM-MD5, XOAUTH2, test email
- [x] Backups: full and incremental, to FTP, FTPS (explicit/implicit) or SCP, scheduled or by hand, optional age encryption, retention
- [x] Restore from the remote location, also on a fresh server
- [x] Info screen with version, update check, production/test channel switch and one-click update from GitHub releases
- [x] CI and release pipeline (test pre-releases from `test`, releases from `main`)

## 2. Windows agent and connectivity (next)

The core of the product: reach a Windows machine behind any NAT router and control it from the browser.

- [ ] Rust agent for Windows 11 and Windows Server 2019/2022/2025, running as a Windows service
- [ ] Device enrollment with a per-customer enrollment key; devices and customers appear in the portal
- [ ] Heartbeat and online/offline status; dashboard counts and "offline for more than 7 days"
- [ ] Outbound-only connection from the agent to the server (WebSocket over 443), so no port forwarding is needed at the client
- [ ] New `gateway` and `relay` server roles: signaling for WebRTC, TURN relay as fallback when a direct connection is not possible
- [ ] Browser viewer in the portal: screen, mouse, keyboard, multi-monitor, clipboard
- [ ] Unattended access with per-device permissions, plus attended mode with a consent prompt
- [ ] Hardware and OS inventory per device

## 3. Quick support client

A one-off client for ad-hoc support, without installation.

- [ ] Single portable `.exe`, no admin rights or install needed
- [ ] Shows a short session code; the technician enters it in the portal
- [ ] The user approves the session and can end it at any time; nothing stays behind after closing
- [ ] Optional upgrade from a quick session to a permanent unattended install

## 4. Session audit and recording (in progress)

Server and portal side are done; recording starts working end to end once the browser viewer from phase 2 calls the recorder. Details in [docs/session-recording.md](docs/session-recording.md).

- [x] Session log: who connected to which device, when, how long and from where, with a timeline per session (connection type, file transfers, elevation, views and shares)
- [x] Filter by device, customer, date, technician; technicians see only their own sessions; CSV export
- [x] Recording policy: off, technician chooses, or always
- [x] Recording in the technician's browser (MediaRecorder, WebM), uploaded in chunks while the session runs, so the agent does no video encoding
- [x] Recordings stored encrypted (AES-256-GCM per chunk) in the data folder, included in backups
- [x] Playback with seeking, download, delete, automatic retention
- [x] Private share links for customers: expiring, revocable, every view logged
- [ ] Hook the recorder into the browser viewer (phase 2)
- [ ] "This session is being recorded" banner on the Windows side (phase 2 agent)
- [ ] Per-customer or per-device recording policy override

## 5. Client builder and central updates

- [ ] Client builder in the portal: company logo, colors, product name and the list of servers the client connects to
- [ ] Separate builds per customer, with the enrollment key baked in
- [ ] The server is the central update source for all agents, with production and test channels
- [ ] Signed update packages (Ed25519) that agents verify before installing
- [ ] Build pipeline prepared for Authenticode signing, so a code signing certificate can be plugged in later (see phase 7)

## 6. Multi-server and scale-out (in progress)

Start on one server, grow without reinstalling. Details in [docs/scaling.md](docs/scaling.md).

- [x] `relay` role: TURN over UDP/TCP and TURNS (TLS, Let's Encrypt on 443) with time-limited credentials and key rotation
- [x] Separate relay servers without a database: one-time join token from the portal, install with `install.sh --role relay`
- [x] Relays follow the control server's version automatically
- [x] Least-busy relay selection, drain for maintenance, remove to revoke
- [x] Relays refuse to forward to private and loopback addresses (no access to the internal network)
- [x] Message bus between servers: in-process on one server, NATS with mutual TLS in a cluster
- [x] Multiple control servers on a shared PostgreSQL; backups and cleanup run on one elected leader with automatic failover
- [x] Servers page in the portal: status, roles, version, load and heartbeat of every server
- [x] Encryption enforced between all servers (HTTPS, NATS mTLS, PostgreSQL TLS)
- [x] Documentation for each step: 1 server, separate relays, full cluster; reverse proxy (Zoraxy) notes
- [ ] Separate `gateway` servers for agent connections, with signaling routed over NATS (with phase 2)
- [ ] Region-aware relay selection (pick a relay close to the customer)
- [ ] Ready-made NATS setup in the installer

## 7. Code signing and open-source release

- [ ] Authenticode code signing certificate in the build pipeline for the agent, quick support client and installers
- [ ] A brand-neutral open-source variant: name, logo and colors fully configurable, no RHIS defaults
- [ ] Install and admin documentation, contribution guide and security policy
- [ ] First stable `v1.0.0` release

## Later

- macOS and Linux agents
- File transfer and remote terminal/PowerShell
- Two-factor authentication (TOTP, WebAuthn) and single sign-on (OIDC)
- Wake-on-LAN via another agent in the same network
