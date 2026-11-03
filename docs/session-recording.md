# Session audit and recording

Every remote session is written to the session log. Recording the screen as video is optional and set by an administrator under **Settings → Session recording**.

## What is logged

For every session the server stores:

- device, customer, technician (name and email) and the technician's IP address. Names are copied into the log, so it stays readable after a device, customer or user is deleted;
- type (unattended, attended, quick support), start, end, duration and who ended it;
- connection type: direct (P2P), relay (TURN) or relay (WebSocket);
- a timeline: start, connection changes, file transfers, clipboard, switch to administrator, chat, end, and every view, download, share and deletion of the recording.

Administrators see all sessions. Technicians see only their own. The list can be filtered by text, customer, date, device, "with recording" and "active now", and exported as CSV (the export is logged in the activity log).

## Recording policy

| Mode | Behaviour |
| --- | --- |
| Never | Nothing is recorded. Sessions are still logged. |
| Technician chooses (default) | The technician turns recording on when starting a session. |
| Always | Every session is recorded. |

Other settings: how long recordings are kept (default 90 days, 0 = forever) and how long new share links stay valid (default 7 days, max 90).

## How recording works

```
Windows agent --(WebRTC, DTLS-SRTP encrypted)--> technician's browser --(HTTPS)--> server
   shows "being recorded" banner               MediaRecorder, 5 s WebM chunks      encrypted chunk files
```

1. The gateway calls `sessions.Service.Start`. The policy plus the technician's choice decide whether the session is recorded; `Session.MustRecord()` tells the gateway, which sets `recording: true` in the session request so the agent shows its banner.
2. The browser viewer records the incoming video (the WebRTC track, or `canvas.captureStream()` for the WebSocket fallback) with `MediaRecorder` and uploads 5-second chunks in order. Failed uploads are retried; the server accepts a repeated chunk, so retries are safe.
3. When the session ends the viewer calls `finish`. A viewer that disappears without finishing leaves the recording `incomplete` (after 15 minutes), and what was uploaded stays playable.

Recording in the browser keeps the agent light (no video encoder on the customer's PC) and means the video never leaves the encrypted channels: WebRTC between agent and browser, HTTPS between browser and server.

## Storage and encryption

Recordings live in `<data_dir>/recordings/<session id>/` as one file per chunk. Every chunk is encrypted with AES-256-GCM; the session id and chunk number are bound into the authentication tag, so chunks cannot be swapped or reordered. The key is derived from `secret_key` in `config.yaml`.

- Keep `secret_key` safe and with your backups. Without it, recordings (and stored passwords) cannot be decrypted.
- The data folder is part of the backups, so recordings are backed up (still encrypted).
- Playback decrypts on the fly and supports HTTP Range requests, so the player can seek.

## Share links for customers

From a session with a recording, **Share with customer** creates a private link `https://<server>/r/<token>`:

- the token is 256 random bits; the server stores only its SHA-256 hash, and the link is shown once;
- links expire (1 to 90 days) and can be revoked at any time; deleting the recording revokes all its links;
- the customer page shows the company name, device, date, duration and an optional note, with play and download, and no internal data such as IP addresses;
- every opening and download is written to the session timeline and the activity log, with the viewer's IP address.

## For developers

Server package: `server/internal/sessions`.

| Call | Use |
| --- | --- |
| `Start(ctx, StartParams{DeviceID, UserID, Kind, Transport, ViewerIP, Record})` | open a session; returns `*Session` with `ID` and `MustRecord()` |
| `SetTransport(ctx, id, "p2p" \| "turn" \| "websocket")` | record the connection type (logged on the timeline) |
| `AddEvent(ctx, id, kind, details)` | timeline entry, e.g. `file_transfer` with `{"name": "..."}`, `clipboard`, `elevation`, `chat` |
| `End(ctx, id, reason)` | close the session; reasons with a translation: `viewer_closed`, `agent_closed`, `timeout`, `server_shutdown` |

Browser: `web/src/recording/useSessionRecorder.ts`.

```ts
const rec = useSessionRecorder(session.id, session.recording === "recording", () => video.srcObject as MediaStream);
// WebSocket fallback: () => canvas.captureStream(15)
await rec.stop(); // before closing the connection
```

API (all under `/api`):

| Method and path | Who |
| --- | --- |
| `GET /sessions`, `GET /sessions/export.csv` | signed in (technicians: own sessions) |
| `GET /sessions/{id}`, `GET /sessions/{id}/recording[?download=1]` | admin or the session's technician |
| `PUT /sessions/{id}/recording/{seq}`, `POST /sessions/{id}/recording/finish` | the session's technician |
| `POST /sessions/{id}/shares`, `DELETE /sessions/{id}/shares/{share}` | admin or the session's technician |
| `DELETE /sessions/{id}/recording`, `GET/PUT /settings/recording` | admin |
| `GET /public/recordings/{token}[/video]` | anyone with a valid link |
