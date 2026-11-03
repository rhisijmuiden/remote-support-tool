# Roadmap

RHIS Remote is built in phases. Each phase ships as test pre-releases (`vX.Y.Z-test.N` from the `test` branch) first and is promoted to a production release (`vX.Y.Z` from `main`) once it has been tested.

Every phase has a tracking issue labelled `roadmap` on GitHub. Detailed tasks are added to that issue when the phase starts.

| Phase | Status | Target version | Issue |
| --- | --- | --- | --- |
| 1. Server foundation | Done | v0.1 | #4 |
| 2. Windows agent and connectivity | Next | v0.2 | #5 |
| 3. Quick support client | Planned | v0.3 | #6 |
| 4. Session audit and recording | In progress | v0.4 | #7 |
| 5. Client builder and central updates | In progress | v0.5 | #8 |
| 6. Multi-server and scale-out | Planned | v0.6 | #9 |
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

## 5. Client builder and central updates (in progress)

Server and portal side are done; agents use them once the Windows agent from phase 2 is in. Details in [docs/client-builder.md](docs/client-builder.md).

- [x] Client builder in the portal: company logo, accent colour, product and company name, and an ordered failover list of servers (https only)
- [x] Separate builds per customer with the enrolment token baked in; installer (unattended) and QuickSupport types; x64 and ARM64
- [x] Expiring, revocable download links to send an installer to a customer; revoke a client or replace its token
- [x] The server is the central update source for all agents: imports agent releases from GitHub or an upload, production and test channels
- [x] Update policy: newest or pinned version per channel (also to roll back), per-customer exceptions, staged rollout percentage
- [x] Signed update manifests (Ed25519, key created by the server and embedded in every client), checked against size and SHA-256
- [x] Authenticode signing on the server: PFX certificate (osslsigncode) or a signing command for hardware tokens, HSMs and cloud signing
- [ ] Agent-side updater in the Windows agent (verify, download, swap, report), once the agent from phase 2 is merged
- [ ] Push "update available" over the agent control channel instead of waiting for the next check

## 6. Multi-server and scale-out

Start on one server, grow without reinstalling. The plan with the order of work is in [docs/multi-server.md](docs/multi-server.md).

- [ ] Run `gateway` and `relay` roles on separate servers, for example relays close to customers
- [ ] NATS message bus between server roles
- [ ] Multiple web/API nodes behind a load balancer, shared PostgreSQL
- [ ] Health and status of every node in the portal
- [ ] Signed server list updates for agents, so adding servers needs no new installers
- [ ] Object storage (S3-compatible) for recordings, installers and agent files; scheduler leader election
- [ ] Documentation for each step: 1 server, separate relays, full cluster

## 7. Code signing and open-source release

- [x] Server-side Authenticode signing of installers and agent updates (phase 5)
- [ ] Authenticode code signing certificate in the build pipeline for the agent, quick support client and installers
- [ ] A brand-neutral open-source variant: name, logo and colors fully configurable, no RHIS defaults
- [ ] Install and admin documentation, contribution guide and security policy
- [ ] First stable `v1.0.0` release

## Later

- macOS and Linux agents
- File transfer and remote terminal/PowerShell
- Two-factor authentication (TOTP, WebAuthn) and single sign-on (OIDC)
- Wake-on-LAN via another agent in the same network
