package cluster

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rhisijmuiden/remote-support-tool/server/internal/secret"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/settings"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/version"
)

// JoinableRoles are the roles a server may get through a join token. Control roles
// (web, api, scheduler) need the database and join by configuration instead.
var JoinableRoles = []string{"relay"}

var (
	ErrNotFound     = errors.New("not_found")
	ErrInvalidToken = errors.New("invalid_token")
	ErrBadRole      = errors.New("invalid_role")
	ErrUnauthorized = errors.New("unauthorized")
)

// ICEServer is one entry for RTCPeerConnection's iceServers.
type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// Registry keeps the list of servers in the database. Every control server has one.
type Registry struct {
	DB    *pgxpool.Pool
	Store *settings.Store
	Box   *secret.Box
	Bus   Bus
	Self  *Identity
	Name  string
	// Region and Roles describe this server.
	Region string
	Roles  []string
	// RelayURLs returns this server's relay URLs when it also runs the relay role.
	RelayURLs func() []string
	Stats     []StatsFunc

	started time.Time
	secrets struct {
		sync.Mutex
		list []string
		at   time.Time
	}
}

// Run registers this server and refreshes its heartbeat until ctx ends.
func (r *Registry) Run(ctx context.Context) {
	r.started = time.Now()
	t := time.NewTicker(HeartbeatInterval)
	defer t.Stop()
	for {
		if err := r.beat(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("cluster heartbeat", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (r *Registry) beat(ctx context.Context) error {
	var urls []string
	if r.RelayURLs != nil {
		urls = r.RelayURLs()
	}
	stats, _ := json.Marshal(mergeStats(SystemStats(r.started), r.Stats))
	_, err := r.DB.Exec(ctx, `INSERT INTO cluster_nodes(id, name, region, roles, version, relay_urls, stats, started_at, last_seen)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,now())
		ON CONFLICT(id) DO UPDATE SET name=EXCLUDED.name, region=EXCLUDED.region, roles=EXCLUDED.roles,
			version=EXCLUDED.version, relay_urls=EXCLUDED.relay_urls, stats=EXCLUDED.stats,
			started_at=EXCLUDED.started_at, last_seen=now()`,
		r.Self.ID, r.Name, r.Region, r.Roles, version.Version, nonNil(urls), stats, r.started)
	return err
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

const nodeCols = `id, name, region, roles, version, address, relay_urls, stats, remote, draining, started_at, last_seen, created_at`

func scanNode(row pgx.Row) (Node, error) {
	var n Node
	var stats []byte
	err := row.Scan(&n.ID, &n.Name, &n.Region, &n.Roles, &n.Version, &n.Address, &n.RelayURLs, &stats,
		&n.Remote, &n.Draining, &n.StartedAt, &n.LastSeen, &n.CreatedAt)
	if err != nil {
		return n, err
	}
	json.Unmarshal(stats, &n.Stats)
	n.Online = time.Since(n.LastSeen) < OfflineAfter
	return n, nil
}

// Nodes lists every registered server, this one first.
func (r *Registry) Nodes(ctx context.Context) ([]Node, error) {
	rows, err := r.DB.Query(ctx, `SELECT `+nodeCols+` FROM cluster_nodes ORDER BY name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Node{}
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		n.Self = n.ID == r.Self.ID
		out = append(out, n)
	}
	slices.SortStableFunc(out, func(a, b Node) int {
		if a.Self != b.Self {
			if a.Self {
				return -1
			}
			return 1
		}
		return 0
	})
	return out, rows.Err()
}

// SetDraining stops (or resumes) handing out a relay for new sessions.
func (r *Registry) SetDraining(ctx context.Context, id string, draining bool) error {
	tag, err := r.DB.Exec(ctx, `UPDATE cluster_nodes SET draining=$2 WHERE id=$1`, id, draining)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	r.publish(EventNodesChanged)
	return nil
}

// Remove deletes a server. A remote relay loses its credential and stops serving.
func (r *Registry) Remove(ctx context.Context, id string) error {
	if id == r.Self.ID {
		return errors.New("cannot_remove_self")
	}
	tag, err := r.DB.Exec(ctx, `DELETE FROM cluster_nodes WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	r.publish(EventNodesChanged)
	return nil
}

// PruneOffline removes control servers that have been gone for a long time; they
// register again when they come back. Remote relays are kept until an admin removes them.
func (r *Registry) PruneOffline(ctx context.Context, after time.Duration) error {
	_, err := r.DB.Exec(ctx, `DELETE FROM cluster_nodes WHERE NOT remote AND last_seen < now() - $1::interval`,
		fmt.Sprintf("%d seconds", int(after.Seconds())))
	return err
}

func (r *Registry) publish(subject string) {
	if r.Bus != nil {
		r.Bus.Publish(subject, nil)
	}
}

// ---- join tokens ----

type JoinToken struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Roles     []string  `json:"roles"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

func hashToken(t string) []byte {
	h := sha256.Sum256([]byte("rhis-remote/cluster-token\x00" + t))
	return h[:]
}

// CreateJoinToken returns a one-time token that lets a new server join.
func (r *Registry) CreateJoinToken(ctx context.Context, name string, roles []string, ttl time.Duration, userID int64) (string, *JoinToken, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return "", nil, errors.New("invalid_name")
	}
	if len(roles) == 0 {
		return "", nil, ErrBadRole
	}
	for _, ro := range roles {
		if !slices.Contains(JoinableRoles, ro) {
			return "", nil, ErrBadRole
		}
	}
	if ttl <= 0 || ttl > 7*24*time.Hour {
		ttl = 24 * time.Hour
	}
	tok := "rj_" + randomToken(24)
	var uid any
	if userID != 0 {
		uid = userID
	}
	jt := &JoinToken{Name: name, Roles: roles}
	err := r.DB.QueryRow(ctx, `INSERT INTO cluster_join_tokens(token_hash, name, roles, created_by, expires_at)
		VALUES($1,$2,$3,$4, now() + $5::interval) RETURNING id, created_at, expires_at`,
		hashToken(tok), name, roles, uid, fmt.Sprintf("%d seconds", int(ttl.Seconds()))).Scan(&jt.ID, &jt.CreatedAt, &jt.ExpiresAt)
	if err != nil {
		return "", nil, err
	}
	return tok, jt, nil
}

// JoinTokens lists tokens that are still usable.
func (r *Registry) JoinTokens(ctx context.Context) ([]JoinToken, error) {
	rows, err := r.DB.Query(ctx, `SELECT id, name, roles, created_at, expires_at FROM cluster_join_tokens
		WHERE used_at IS NULL AND expires_at > now() ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []JoinToken{}
	for rows.Next() {
		var t JoinToken
		if err := rows.Scan(&t.ID, &t.Name, &t.Roles, &t.CreatedAt, &t.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r *Registry) RevokeJoinToken(ctx context.Context, id int64) error {
	tag, err := r.DB.Exec(ctx, `DELETE FROM cluster_join_tokens WHERE id=$1 AND used_at IS NULL`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- remote servers (relays without a database) ----

// JoinRequest is sent by a new server together with its join token.
type JoinRequest struct {
	Token   string   `json:"token"`
	NodeID  string   `json:"node_id"`
	Name    string   `json:"name"`
	Region  string   `json:"region"`
	Version string   `json:"version"`
	Roles   []string `json:"roles"`
}

type JoinResponse struct {
	NodeID     string   `json:"node_id"`
	Credential string   `json:"credential"`
	Roles      []string `json:"roles"`
	HeartbeatResponse
}

// HeartbeatRequest is sent by a remote server every HeartbeatInterval.
type HeartbeatRequest struct {
	Name      string         `json:"name"`
	Region    string         `json:"region"`
	Version   string         `json:"version"`
	Roles     []string       `json:"roles"`
	RelayURLs []string       `json:"relay_urls"`
	Stats     map[string]any `json:"stats"`
	StartedAt time.Time      `json:"started_at"`
}

type HeartbeatResponse struct {
	// TURNSecrets are the shared secrets for checking TURN credentials, newest first.
	TURNSecrets []string `json:"turn_secrets"`
	// Version is the control server's version; remote servers update to match it.
	Version  string `json:"version"`
	Channel  string `json:"channel"`
	Draining bool   `json:"draining"`
	// ObservedIP is the address the control server saw the request come from.
	ObservedIP string `json:"observed_ip"`
}

// Join exchanges a one-time token for a node credential. The roles come from the token,
// so a server cannot give itself more rights than the administrator granted.
func (r *Registry) Join(ctx context.Context, req JoinRequest, addr string) (*JoinResponse, error) {
	if req.Token == "" {
		return nil, ErrInvalidToken
	}
	tx, err := r.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var tokID int64
	var name string
	var roles []string
	err = tx.QueryRow(ctx, `UPDATE cluster_join_tokens SET used_at=now()
		WHERE token_hash=$1 AND used_at IS NULL AND expires_at > now() RETURNING id, name, roles`,
		hashToken(req.Token)).Scan(&tokID, &name, &roles)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrInvalidToken
	}
	if err != nil {
		return nil, err
	}
	id := req.NodeID
	if !validNodeID(id) {
		id = newID()
	}
	// A reused ID from an earlier installation gets a fresh one rather than taking over.
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM cluster_nodes WHERE id=$1)`, id).Scan(&exists); err != nil {
		return nil, err
	}
	if exists {
		id = newID()
	}
	cred := "rc_" + randomToken(32)
	if _, err := tx.Exec(ctx, `INSERT INTO cluster_nodes(id, name, region, roles, version, address, remote, credential_hash, last_seen)
		VALUES($1,$2,$3,$4,$5,$6,true,$7, now() - interval '1 hour')`,
		id, name, clip(req.Region, 64), roles, clip(req.Version, 64), addr, hashToken(cred)); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE cluster_join_tokens SET node_id=$2 WHERE id=$1`, tokID, id); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	hb, err := r.heartbeatResponse(ctx, false, addr)
	if err != nil {
		return nil, err
	}
	r.publish(EventNodesChanged)
	return &JoinResponse{NodeID: id, Credential: cred, Roles: roles, HeartbeatResponse: *hb}, nil
}

func validNodeID(id string) bool {
	if !strings.HasPrefix(id, "n_") || len(id) != 22 {
		return false
	}
	for _, c := range id[2:] {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// Authenticate checks a remote server's "<node id>.<credential>" bearer token.
func (r *Registry) Authenticate(ctx context.Context, bearer string) (string, error) {
	id, cred, ok := strings.Cut(bearer, ".")
	if !ok || !validNodeID(id) || cred == "" {
		return "", ErrUnauthorized
	}
	var h []byte
	err := r.DB.QueryRow(ctx, `SELECT credential_hash FROM cluster_nodes WHERE id=$1 AND remote`, id).Scan(&h)
	if err != nil || subtle.ConstantTimeCompare(h, hashToken(cred)) != 1 {
		return "", ErrUnauthorized
	}
	return id, nil
}

// Heartbeat records a remote server's status and returns what it needs to keep running.
func (r *Registry) Heartbeat(ctx context.Context, id string, req HeartbeatRequest, addr string) (*HeartbeatResponse, error) {
	stats, _ := json.Marshal(req.Stats)
	if len(stats) > 16<<10 {
		stats = []byte("{}")
	}
	started := req.StartedAt
	if started.IsZero() || started.After(time.Now().Add(time.Minute)) {
		started = time.Now()
	}
	var draining bool
	// The name and roles stay as the administrator set them.
	err := r.DB.QueryRow(ctx, `UPDATE cluster_nodes SET region=$2, version=$3, relay_urls=$4, stats=$5,
		address=$6, started_at=$7, last_seen=now() WHERE id=$1 AND remote RETURNING draining`,
		id, clip(req.Region, 64), clip(req.Version, 64), nonNil(limitURLs(req.RelayURLs)), stats, addr, started).Scan(&draining)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUnauthorized
	}
	if err != nil {
		return nil, err
	}
	return r.heartbeatResponse(ctx, draining, addr)
}

func limitURLs(u []string) []string {
	out := []string{}
	for _, s := range u {
		if len(out) == 8 {
			break
		}
		if len(s) <= 200 && (strings.HasPrefix(s, "stun:") || strings.HasPrefix(s, "turn:") || strings.HasPrefix(s, "turns:")) {
			out = append(out, s)
		}
	}
	return out
}

func (r *Registry) heartbeatResponse(ctx context.Context, draining bool, addr string) (*HeartbeatResponse, error) {
	sec, err := r.TURNSecrets(ctx)
	if err != nil {
		return nil, err
	}
	return &HeartbeatResponse{TURNSecrets: sec, Version: version.Version, Channel: version.Channel, Draining: draining, ObservedIP: addr}, nil
}

// ---- TURN secrets and relay selection ----

const turnSecretKey = "cluster.turn"

type turnSettings struct {
	// Secrets are sealed with the server's secret_key, newest first.
	Secrets []string `json:"secrets"`
}

// TURNSecrets returns the current and previous TURN secret, creating one if needed.
func (r *Registry) TURNSecrets(ctx context.Context) ([]string, error) {
	r.secrets.Lock()
	defer r.secrets.Unlock()
	if r.secrets.list != nil && time.Since(r.secrets.at) < 15*time.Second {
		return r.secrets.list, nil
	}
	var st turnSettings
	if _, err := r.Store.Get(ctx, turnSecretKey, &st); err != nil {
		return nil, err
	}
	if len(st.Secrets) == 0 {
		if err := r.rotateLocked(ctx, &st); err != nil {
			return nil, err
		}
	}
	out := []string{}
	for _, s := range st.Secrets {
		p, err := r.Box.Open(s)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	r.secrets.list, r.secrets.at = out, time.Now()
	return out, nil
}

// RotateTURNSecret creates a new secret. The previous one keeps working until the
// credentials signed with it expire.
func (r *Registry) RotateTURNSecret(ctx context.Context) error {
	r.secrets.Lock()
	defer r.secrets.Unlock()
	var st turnSettings
	if _, err := r.Store.Get(ctx, turnSecretKey, &st); err != nil {
		return err
	}
	return r.rotateLocked(ctx, &st)
}

func (r *Registry) rotateLocked(ctx context.Context, st *turnSettings) error {
	sealed, err := r.Box.Seal(randomToken(32))
	if err != nil {
		return err
	}
	st.Secrets = append([]string{sealed}, st.Secrets...)
	if len(st.Secrets) > 2 {
		st.Secrets = st.Secrets[:2]
	}
	if err := r.Store.Put(ctx, turnSecretKey, st); err != nil {
		return err
	}
	r.secrets.list = nil
	return nil
}

// MaxRelaysPerSession is how many relays a viewer or agent is offered at once.
const MaxRelaysPerSession = 2

// ICEServers returns STUN/TURN servers for a new session: the least busy online relays
// that are not draining, each with a fresh time-limited credential. userTag ends up in
// the TURN username (for example "u12" or "d345") so relay logs can be traced back.
func (r *Registry) ICEServers(ctx context.Context, userTag string) ([]ICEServer, error) {
	rows, err := r.DB.Query(ctx, `SELECT relay_urls FROM cluster_nodes
		WHERE 'relay' = ANY(roles) AND NOT draining AND cardinality(relay_urls) > 0
		  AND last_seen > now() - $1::interval
		ORDER BY COALESCE((stats->>'allocations')::int, 0), random() LIMIT $2`,
		fmt.Sprintf("%d seconds", int(OfflineAfter.Seconds())), MaxRelaysPerSession)
	if err != nil {
		return nil, err
	}
	var urls [][]string
	for rows.Next() {
		var u []string
		if err := rows.Scan(&u); err != nil {
			rows.Close()
			return nil, err
		}
		urls = append(urls, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(urls) == 0 {
		return []ICEServer{}, nil
	}
	sec, err := r.TURNSecrets(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ICEServer, 0, len(urls))
	for _, u := range urls {
		user, pass := NewTURNCredential(sec[0], userTag, time.Now(), CredentialTTL)
		out = append(out, ICEServer{URLs: u, Username: user, Credential: pass})
	}
	return out, nil
}
