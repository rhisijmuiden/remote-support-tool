# Roadmap

RHIS Remote is built in phases. Each phase ships as test pre-releases (`vX.Y.Z-test.N` from the `test` branch) first and is promoted to a production release (`vX.Y.Z` from `main`) once it has been tested.

Every phase has a tracking issue labelled `roadmap` on GitHub. Detailed tasks are added to that issue when the phase starts.

| Phase | Status | Target version | Issue |
| --- | --- | --- | --- |
| 1. Server foundation | Done | v0.1 | #4 |
| 2. Windows agent and connectivity | In test | v0.2 | #5 |
| 3. Quick support client | In test | v0.3 | #6 |
| 4. Session audit and recording | In progress | v0.4 | #7 |
| 5. Client builder and central updates | In progress | v0.5 | #8 |
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

## 2. Windows agent and connectivity (in test)

The core of the product: reach a Windows machine behind any NAT router and control it from the browser. In test as v0.2; the Windows build compiles and passes CI but still needs a run on real Windows 11 and Windows Server hardware.

- [x] Rust agent for Windows 11 and Windows Server 2019/2022/2025, running as a Windows service (LocalSystem, restart on failure)
- [x] Device enrollment with an enrollment token, optionally tied to a customer; devices and customers appear in the portal
- [x] Heartbeat and online/offline status; dashboard counts and "offline for more than 7 days"
- [x] Outbound-only connection from the agent to the server (WSS over 443), so no port forwarding is needed at the client; works behind a reverse proxy such as Zoraxy
- [x] All agent traffic encrypted: TLS only (optional certificate pin), DTLS-SRTP media, tokens stored hashed on the server and DPAPI-encrypted on the PC
- [x] New `gateway` and `relay` server roles: signaling for WebRTC, TURN relay as fallback, and an encrypted WebSocket relay over 443 as last resort
- [x] Browser viewer in the portal: screen, mouse, keyboard, multi-monitor, Ctrl+Alt+Del, sign-in and UAC screens, view-only, full screen
- [x] Unattended access, with a banner for the local user while a session runs
- [x] Build-signing hook for the agent (Authenticode certificate can be plugged in via repository secrets)
- [ ] Test on real Windows 11 and Windows Server hardware
- [ ] Clipboard sync and file transfer
- [ ] Per-device permissions and attended mode with a consent prompt
- [ ] Hardware and OS inventory per device (hostname, OS version and last user are in already)
- [ ] Faster capture with DXGI Desktop Duplication (GDI for now)

## 3. Quick support client (in test)

A one-off client for ad-hoc support, without installation. Merged in #14; details in [docs/quicksupport.md](docs/quicksupport.md). Not yet run on real Windows hardware.

- [x] Single portable `.exe`, no admin rights or install needed
- [x] Shows a 9-digit session code; the technician enters it in the portal (Quick support page)
- [x] The user approves every session and can end it at any time; closing the window retires the code
- [x] Server download with the server address, branding and update keys embedded; client builder QuickSupport builds
- [x] Info dialog with version and signed updates
- [ ] Test on Windows 11 and Windows Server hardware
- [ ] Optional upgrade from a quick session to a permanent unattended install

## 4. Session audit and recording (in progress)

Server, portal and the browser viewer are wired up; agent sessions are logged and recorded through this service. Details in [docs/session-recording.md](docs/session-recording.md).

- [x] Session log: who connected to which device, when, how long and from where, with a timeline per session (connection type, file transfers, elevation, views and shares)
- [x] Filter by device, customer, date, technician; technicians see only their own sessions; CSV export
- [x] Recording policy: off, technician chooses, or always
- [x] Recording in the technician's browser (MediaRecorder, WebM), uploaded in chunks while the session runs, so the agent does no video encoding
- [x] Recordings stored encrypted (AES-256-GCM per chunk) in the data folder, included in backups
- [x] Playback with seeking, download, delete, automatic retention
- [x] Private share links for customers: expiring, revocable, every view logged
- [x] Hook the recorder into the browser viewer (phase 2)
- [x] "This session is being recorded" banner on the Windows side (phase 2 agent)
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
- [ ] Signed server list updates for agents over the control channel, so adding or moving servers needs no new installers

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
