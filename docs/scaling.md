# Scaling RHIS Remote

RHIS Remote is one binary. The `roles` setting decides what a server does, so an
installation can start on one machine and grow without reinstalling.

| Role | Needs the database | What it does |
| --- | --- | --- |
| `web` | yes | Serves the portal |
| `api` | yes | JSON API for the portal; relays join and send heartbeats here |
| `scheduler` | yes | Scheduled backups and cleanup. In a cluster only the elected leader runs it |
| `relay` | no | TURN/TURNS media relay for sessions that cannot connect directly |

`roles: ["all"]` turns on every role. The gateway role for agent connections is added with
the Windows agent (roadmap phase 2).

All servers are listed in the portal under **Settings > Servers** with their roles, status,
version, load and last heartbeat.

## Encryption between servers

Every connection between servers is encrypted, and the server refuses to start with a
configuration that would send anything in plain text:

- Relays talk to the control server over **HTTPS** only (`join.control_url` must be `https://`).
- NATS between control servers requires **TLS with client certificates** (`tls://` URLs plus `cert_file` and `key_file`).
- A PostgreSQL database on another machine requires `sslmode=require`, `verify-ca` or `verify-full` (use `verify-full`).
- Media through a relay is **end-to-end encrypted** by WebRTC (DTLS-SRTP); the relay only forwards
  encrypted packets and never sees the screen. TURN credentials are time limited (24 hours) and
  signed with a secret only the servers know. TURNS (TURN over TLS) is offered when the relay has
  a certificate, for networks that only allow port 443.

`allow_insecure_http: true` switches these checks off. It exists for lab setups only.

## Step 1: one server

The default installation. Portal, API, scheduler and relay run on one Debian 13 machine.

```bash
sudo bash install.sh --domain remote.example.com --public-ip 203.0.113.4
```

Ports to open or forward to this server:

| Port | Protocol | Purpose |
| --- | --- | --- |
| 443 | TCP | Portal and API (HTTPS) |
| 3478 | UDP + TCP | TURN relay |
| 49160-49359 | UDP | Relayed media (one port per relayed connection) |

`--public-ip` is the address customers reach the relay on. Without it the server uses the
address of the `public_url` host name; when that is a private address (split DNS, NAT) the relay
does not start and the log says so.

### Behind a reverse proxy (Zoraxy, nginx, Traefik)

The portal and API work behind any HTTPS reverse proxy. Install with
`--domain <public name> --behind-proxy <proxy IP>` (or add the proxy's address to
`trusted_proxies`) so the real client address is used for the sign-in limit, the activity log
and for relays that register through the proxy.

TURN is not HTTP and cannot go through an HTTP reverse proxy. Forward **3478 UDP+TCP and
49160-49359 UDP directly** from the router to the RHIS Remote server, past the proxy. Zoraxy's
stream proxy (TCP/UDP) also works for 3478, but the relayed port range must reach the server
with the same port numbers, so a plain port forward is simpler. TURNS on 443 is not available
on a server where the proxy owns port 443; use a separate relay with `--relay-domain` for that.

## Step 2: separate relays

Put relays close to your customers, or add capacity, without touching the database.

1. In the portal open **Settings > Servers > Add relay server**. Give it a name and, optionally,
   a DNS name that points at the new server.
2. Run the command shown on a fresh Debian 13 machine:

   ```bash
   curl -fsSL https://raw.githubusercontent.com/rhisijmuiden/remote-support-tool/main/deploy/debian/install.sh \
     | sudo bash -s -- --role relay --channel production --control https://remote.example.com --token rj_... --relay-domain relay1.example.com
   ```

3. Open 3478 UDP+TCP, 49160-49359 UDP and, with a DNS name, 443 TCP to the relay.

What happens:

- The relay exchanges the one-time token for its own credential (stored in
  `/var/lib/rhis-remote/data/node.json`) and from then on sends a heartbeat every 10 seconds.
  The roles come from the token; a relay cannot give itself more rights.
- With a DNS name it gets a Let's Encrypt certificate (TLS-ALPN on port 443) and offers TURNS on 443.
- The relay updates itself to the version the control server runs, so updating the control
  server from the Info screen updates every relay.
- New sessions get the two least busy online relays. **Drain** a relay before maintenance:
  running sessions continue, new ones use other relays. **Remove** revokes its credential; the
  relay stops and does not restart.
- **Rotate relay key** creates a new TURN secret; the previous one stays valid until the
  credentials signed with it expire.
- By default a relay refuses to forward to private, loopback and link-local addresses, so it
  cannot be used to reach the network it runs in. Set `relay.allow_private_peers: true` only when
  viewers and devices share a LAN with the relay.

Tokens are valid for 1 hour to 7 days and can be revoked under "Waiting to join".

## Step 3: cluster of control servers

For high availability or many concurrent technicians, run several control servers.

1. **Shared PostgreSQL** (PostgreSQL 17, replicated or managed) reachable over TLS:
   `database_url: "postgres://rhis_remote:...@db.internal/rhis_remote?sslmode=verify-full"`.
2. **NATS** with TLS and client certificates, three nodes for a quorum. Debian ships `nats-server`.
   Every control server gets the same `cluster.nats` block with its own client certificate.
3. The **same `secret_key`** on every control server: it decrypts stored passwords, recordings
   and the TURN secret.
4. A **shared data folder** (`data_dir`, for example NFS over a private network) so recordings
   and backups are available on every server.
5. A load balancer or reverse proxy in front of the `web`/`api` servers. Sessions are stored in
   the database, so no sticky sessions are needed.

```yaml
roles: ["web", "api", "scheduler"]
node:
  name: "control-2"
  region: "eu-west"
cluster:
  nats:
    url: "tls://nats1.internal:4222,tls://nats2.internal:4222,tls://nats3.internal:4222"
    ca_file: "/etc/rhis-remote/nats/ca.pem"
    cert_file: "/etc/rhis-remote/nats/control-2.pem"
    key_file: "/etc/rhis-remote/nats/control-2-key.pem"
```

Every control server registers itself under Settings > Servers. Database migrations take a
lock, so servers can be updated one by one. Scheduled backups and cleanup run on exactly one
server: the first to take a PostgreSQL advisory lock. If that server goes down, another one
takes over within about 15 seconds. Saving backup settings on any server reloads the schedule
on the leader through NATS.

Control servers that have been offline for 7 days are removed from the list automatically;
they register again when they come back. Joined relays stay until an administrator removes them.

## For developers

- `internal/cluster`: server registry, join tokens, TURN credentials (`ICEServers`), leader
  election (`RunAsLeader`) and the message bus (`Bus`, in-process `LocalBus` or `NATSBus`).
- `internal/relay`: the TURN server (pion/turn).
- Bus subjects: `rhis.dev.<deviceID>.sig` for signaling to the server that holds a device's
  connection, `rhis.cluster.*` for cluster events.
