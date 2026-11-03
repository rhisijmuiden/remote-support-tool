# Multi-server plan

Status: plan for roadmap phase 6. Nothing here is built yet, except where marked as already in place.

RHIS Remote starts on one Debian server and should grow without reinstalling clients or the server. This page describes the steps, what each one needs, and the order to build them in.

## What already helps

- **One binary, several roles.** `roles: ["all"]` today means `web`, `api`, `scheduler` and `update`; the Windows agent adds `gateway` (agent control channel and signaling) and `relay`. Any machine can run any subset.
- **Shared state lives in PostgreSQL.** Settings, the update signing key, enrolment tokens and download links are in the database (secrets encrypted with `secret_key`), so every node sees the same data. Migrations take an advisory lock, so two nodes never migrate at the same time.
- **Clients know several servers.** Every client build carries an ordered failover list of `https://` servers.
- **Content-addressed agent files.** Agent releases are stored by SHA-256, which maps directly onto object storage later.

## Step 1: one server (today)

Everything on one Debian 13 machine behind Caddy: PostgreSQL, the binary with `roles: ["all"]`, the data folder in `/var/lib/rhis-remote`. Backups go off-site with FTP, FTPS or SCP. This carries a few hundred devices; the limit is relay bandwidth for sessions that cannot connect directly.

## Step 2: separate relays

Most sessions connect peer to peer through WebRTC. The ones that cannot (strict NAT, firewalls that only allow 443) go through a relay, which costs bandwidth on the relay. So relays are the first thing to move out, ideally close to customers.

- A **relay node** runs only `roles: ["relay"]`: TURN on UDP 3478 and TURN over TLS on 443, plus the WebSocket fallback. It needs no database access.
- A new **`nodes` table** lists every node: id, roles, public address, region, version, last heartbeat. Nodes register and send a heartbeat every 15 seconds.
- **Short-lived TURN credentials** (the TURN REST scheme: HMAC over an expiry time with a shared secret), so relays never see user accounts and a leaked credential stops working within minutes.
- The gateway hands each session the **ICE servers of healthy relays**, nearest region first.
- Portal: a **Servers** page with every node, its roles, version, heartbeat and load.

## Step 3: a cluster

Several nodes run `web`, `api` and `gateway` behind a load balancer, with one PostgreSQL.

| Concern | Plan |
| --- | --- |
| Agent connected to node A, technician on node B | **NATS** between nodes. A gateway subscribes to `device.<id>` for each agent it holds; the API publishes session requests there. Presence (which node holds which device) in a NATS key-value bucket, with the database `online` flag kept for the portal. |
| Session pairing | The session socket for both sides goes to the node that holds the agent; the API answers the browser with that node's address, or the load balancer routes by session id. |
| Files (recordings, installers, agent releases) | **S3-compatible object storage** (for example Garage or MinIO on own hardware) instead of the local data folder, behind a small storage interface. The single-server setup keeps using the local folder. |
| Scheduled jobs (backups, release import, clean-up) | Only one node runs them: leader election with a PostgreSQL advisory lock held by the active scheduler. |
| PostgreSQL | One primary with a streaming replica for failover; nodes connect through a single address. |
| Server updates | Rolling: one node at a time from the portal. Migrations must stay backward compatible for one release (add first, remove a release later), so old and new nodes can run side by side. |
| Backups | The database dump runs once from the scheduler node; object storage gets its own replication or backup. |
| `secret_key` | The same on every node (it decrypts stored secrets). The installer gets a `--join` mode that copies it and the database address from an existing node. |

## Clients when servers change

Today the server list is fixed in the installer. For a cluster that grows, that is not enough:

- **Signed server list updates.** The server sends the current list on the agent's control channel, signed with the same Ed25519 key as updates. The agent stores it in `agent.json` and uses it from then on. Adding or moving servers then needs no new installers.
- **One public name per role.** Clients use a name like `remote.example.com` that points to the load balancer; the list is only for failover between sites.
- **Region hints.** A client build can name a preferred region, so its agents use the nearest gateway and relays.

## Order of work

1. `nodes` table, heartbeat and the Servers page in the portal.
2. Relay role on its own machine with short-lived TURN credentials; installer option `--roles relay`.
3. Signed server list updates for agents.
4. Storage interface with a local and an S3 backend; move recordings, installers and agent files behind it.
5. NATS between nodes, presence and session routing; scheduler leader election.
6. `install.sh --join`, rolling updates from the portal, and documentation for each step.

Each step ships as its own test release and works on its own, so a single-server installation never has to change.
