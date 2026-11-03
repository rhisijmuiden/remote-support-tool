# Agent protocol

How a Windows agent (or the QuickSupport client) talks to the server, and how a technician's browser reaches it.
Status: v1, used by the Windows agent from step 3.

## Transport and encryption

- Every connection between a client and the server is TLS (HTTPS / WSS). The agent refuses `http://` and `ws://` server URLs.
  The only exception is the `--insecure-dev` flag of a development build, which also requires a loopback address.
- The agent checks the server certificate against the Windows certificate store and the Mozilla root list.
  For a server without a public certificate, the agent can pin the certificate instead (`server_cert_sha256`).
- Screen and input travel over WebRTC, which is always encrypted with DTLS-SRTP (media) and DTLS-SCTP (input data channel).
  A TURN relay only forwards encrypted packets and cannot read them.
- If WebRTC cannot connect, the session falls back to a relay over the session WebSocket (WSS on 443).

## Endpoints

All agent endpoints live under `/api/agent/v1/` and are outside the portal CSRF and cookie checks.
Agents authenticate with `Authorization: Bearer <device_token>`.

| Method | Path | Who | Purpose |
| --- | --- | --- | --- |
| POST | `/api/agent/v1/enroll` | agent | Trade an enrollment token for a device token |
| GET (WS) | `/api/agent/v1/control` | agent | Long-lived control channel: presence and session requests |
| GET (WS) | `/api/agent/v1/sessions/{session_id}` | agent session helper | Signaling and relay fallback for one session |
| POST | `/api/devices/{id}/sessions` | portal user | Start a session with a device |
| GET (WS) | `/api/live/{session_id}/ws` | portal user | Signaling and relay fallback, browser side |

### Enrollment

```http
POST /api/agent/v1/enroll
{"enroll_token": "…", "machine_id": "…", "hostname": "PC-01", "os_version": "Windows 11 Pro 24H2 (26100)", "agent_version": "0.2.0", "kind": "agent"}
→ 200 {"device_id": 12, "device_code": "123 456 789", "device_token": "…"}
```

- `enroll_token` comes from the portal (Devices, Add device) or from a client build (step 4).
  The server tries its own `enrollment_tokens` table first, then any registered `agents.EnrollResolver`.
- `machine_id` is the Windows `MachineGuid`. Enrolling the same machine again replaces its token and keeps the device row.
- The device token is 32 random bytes (base64url). The server only stores its SHA-256.
  The agent stores it in `%ProgramData%\RHIS Remote\agent.json`, readable only by SYSTEM and Administrators, encrypted with DPAPI (machine scope).

### Control channel

`wss://<server>/api/agent/v1/control`, JSON text frames, each with a `type`.
The agent reconnects with exponential backoff (1 s up to 60 s, with jitter). The device is online while this socket is open.

Agent to server:

| type | fields |
| --- | --- |
| `hello` | `hostname`, `os_version`, `agent_version`, `user` (signed-in console user or empty), `machine_id` |
| `status` | `user` (sent when the console user changes) |
| `session_accept` | `session_id` |
| `session_reject` | `session_id`, `reason` (`busy`, `denied`, `error`) |
| `pong` | |

Server to agent:

| type | fields |
| --- | --- |
| `welcome` | `device_id`, `device_code`, `ping_interval` (seconds) |
| `session_request` | `session_id`, `kind` (`unattended`, `attended`, `quick`), `user_name`, `recording` (bool), `ice_servers` |
| `session_cancel` | `session_id` |
| `ping` | |

On `session_request` the agent either answers `session_accept` and opens the session socket, or `session_reject`.
An attended or quick session waits for the local user to click Allow first. When `recording` is true the agent shows that the session is recorded.

### Session socket

Both sides of a session connect to their session socket. The server pairs them and forwards text frames between them unchanged,
and binary frames too (relay fallback). Text frames are JSON with a `type`:

| type | from | fields |
| --- | --- | --- |
| `ready` | server | `peer` (`agent` or `viewer`), sent when both sides are connected |
| `offer` / `answer` | viewer / agent | `sdp` |
| `ice` | both | `candidate`, `sdp_mid`, `sdp_mline_index` (null candidate = end of candidates) |
| `relay` | viewer | Switch to the WebSocket relay (WebRTC failed or was too slow) |
| `keyframe` | viewer | Ask for a new key frame (relay mode) |
| `input` | viewer | Input event, relay mode only (see below) |
| `screen` | agent | `width`, `height`, `monitors` (list of `{index, name, width, height, primary}`), `monitor` |
| `end` | both or server | `reason` |

The viewer makes the WebRTC offer with one receive-only video transceiver and a data channel named `input`.
The agent answers with an H.264 (constrained baseline) video track of the screen.

In relay mode the agent sends each encoded H.264 access unit (Annex B) as a binary frame:
`[1 byte flags: bit0 = key frame][8 bytes timestamp in microseconds, big endian][payload]`.
The browser decodes it with WebCodecs.

### Input events

Sent on the `input` data channel as JSON, or as `{"type":"input", ...}` text frames in relay mode.
Coordinates are normalised to 0..1 of the shown monitor.

| t | fields |
| --- | --- |
| `move` | `x`, `y` |
| `button` | `button` (0 left, 1 middle, 2 right), `down` |
| `wheel` | `dx`, `dy` (pixels, browser convention) |
| `key` | `code` (KeyboardEvent.code), `down` |
| `cad` | Send Ctrl+Alt+Del (secure attention sequence) |
| `monitor` | `index` |

## Windows agent layout

- `rhis-agent.exe` runs as the `RHISRemoteAgent` service (LocalSystem). It holds the control channel.
- For each session the service starts `rhis-agent.exe session` in the active console session with the SYSTEM token,
  so it can capture the sign-in screen and UAC prompts. That process opens the session socket and runs WebRTC.
- QuickSupport uses the same `rhis-core` crate in-process, without a service.
