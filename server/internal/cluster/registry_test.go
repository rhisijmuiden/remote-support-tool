package cluster

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rhisijmuiden/remote-support-tool/server/internal/db"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/secret"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/settings"
)

// testPool uses its own schema so it can run in parallel with other packages' tests.
func testPool(t *testing.T) *pgxpool.Pool {
	url := os.Getenv("RHIS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("RHIS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := db.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	admin.Exec(ctx, `DROP SCHEMA IF EXISTS cluster_test CASCADE`)
	if _, err := admin.Exec(ctx, `CREATE SCHEMA cluster_test`); err != nil {
		t.Fatal(err)
	}
	admin.Close()
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	pool, err := db.Open(ctx, url+sep+"search_path=cluster_test")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func testRegistry(t *testing.T) *Registry {
	pool := testPool(t)
	box, _ := secret.New(strings.Repeat("k", 32))
	return &Registry{DB: pool, Store: &settings.Store{DB: pool}, Box: box, Bus: NewLocalBus(),
		Self: &Identity{ID: newID()}, Name: "control-1", Roles: []string{"web", "api"}}
}

func TestJoinHeartbeatAndICE(t *testing.T) {
	ctx := context.Background()
	r := testRegistry(t)
	if err := r.beat(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.CreateJoinToken(ctx, "relay", []string{"api"}, time.Hour, 0); !errors.Is(err, ErrBadRole) {
		t.Fatalf("control roles must not be joinable, got %v", err)
	}
	tok, _, err := r.CreateJoinToken(ctx, "relay-ams", []string{"relay"}, time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	// The roles come from the token, not from the request.
	jr, err := r.Join(ctx, JoinRequest{Token: tok, Name: "evil", Roles: []string{"web", "api"}}, "203.0.113.5")
	if err != nil {
		t.Fatal(err)
	}
	if len(jr.Roles) != 1 || jr.Roles[0] != "relay" || len(jr.TURNSecrets) != 1 || jr.ObservedIP != "203.0.113.5" {
		t.Fatalf("join response: %+v", jr)
	}
	if _, err := r.Join(ctx, JoinRequest{Token: tok}, "x"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("token reuse: %v", err)
	}
	if _, err := r.Authenticate(ctx, jr.NodeID+".wrong"); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("wrong credential accepted")
	}
	id, err := r.Authenticate(ctx, jr.NodeID+"."+jr.Credential)
	if err != nil || id != jr.NodeID {
		t.Fatalf("authenticate: %v", err)
	}

	// Not online yet: no relays offered.
	ice, err := r.ICEServers(ctx, "u1")
	if err != nil || len(ice) != 0 {
		t.Fatalf("ice before heartbeat: %v %v", ice, err)
	}
	urls := []string{"stun:203.0.113.5:3478", "turn:203.0.113.5:3478?transport=udp", "http://bad"}
	if _, err := r.Heartbeat(ctx, id, HeartbeatRequest{Version: "v1", RelayURLs: urls, Stats: map[string]any{"allocations": 3}}, "203.0.113.5"); err != nil {
		t.Fatal(err)
	}
	ice, err = r.ICEServers(ctx, "u1")
	if err != nil || len(ice) != 1 || len(ice[0].URLs) != 2 {
		t.Fatalf("ice: %+v %v", ice, err)
	}
	sec, _ := r.TURNSecrets(ctx)
	if s, err := TURNSecretFor(ice[0].Username, sec, time.Now()); err != nil || TURNPassword(s, ice[0].Username) != ice[0].Credential {
		t.Fatal("ICE credential does not validate")
	}

	nodes, err := r.Nodes(ctx)
	if err != nil || len(nodes) != 2 || !nodes[0].Self || nodes[1].Name != "relay-ams" || !nodes[1].Online {
		t.Fatalf("nodes: %+v %v", nodes, err)
	}

	// Draining removes the relay from new sessions.
	var events atomic.Int32
	r.Bus.Subscribe(EventNodesChanged, func(Msg) { events.Add(1) })
	if err := r.SetDraining(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if ice, _ := r.ICEServers(ctx, "u1"); len(ice) != 0 {
		t.Fatal("draining relay still offered")
	}
	if hb, _ := r.Heartbeat(ctx, id, HeartbeatRequest{}, "x"); !hb.Draining {
		t.Fatal("heartbeat should report draining")
	}

	// Rotation keeps the previous secret valid.
	if err := r.RotateTURNSecret(ctx); err != nil {
		t.Fatal(err)
	}
	sec2, _ := r.TURNSecrets(ctx)
	if len(sec2) != 2 || sec2[1] != sec[0] {
		t.Fatalf("rotation: %v", sec2)
	}

	if err := r.Remove(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Authenticate(ctx, jr.NodeID+"."+jr.Credential); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("removed node still authenticates")
	}
	if err := r.Remove(ctx, r.Self.ID); err == nil {
		t.Fatal("removed itself")
	}
	if events.Load() != 2 {
		t.Fatalf("want 2 node events, got %d", events.Load())
	}
}

func TestJoinTokenExpiryAndRevoke(t *testing.T) {
	ctx := context.Background()
	r := testRegistry(t)
	tok, jt, err := r.CreateJoinToken(ctx, "relay", []string{"relay"}, time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	list, _ := r.JoinTokens(ctx)
	if len(list) != 1 {
		t.Fatalf("tokens: %v", list)
	}
	if err := r.RevokeJoinToken(ctx, jt.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Join(ctx, JoinRequest{Token: tok}, "x"); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("revoked token accepted")
	}
	tok, _, _ = r.CreateJoinToken(ctx, "relay", []string{"relay"}, time.Hour, 0)
	r.DB.Exec(ctx, `UPDATE cluster_join_tokens SET expires_at = now() - interval '1 second'`)
	if _, err := r.Join(ctx, JoinRequest{Token: tok}, "x"); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("expired token accepted")
	}
}

func TestLeaderElection(t *testing.T) {
	pool := testPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var leaders atomic.Int32
	var max atomic.Int32
	run := func(ctx context.Context) {
		n := leaders.Add(1)
		if n > max.Load() {
			max.Store(n)
		}
		<-ctx.Done()
		leaders.Add(-1)
	}
	ctxA, stopA := context.WithCancel(ctx)
	go RunAsLeader(ctxA, pool, 999001, "test", run)
	go RunAsLeader(ctx, pool, 999001, "test", run)
	waitFor(t, func() bool { return leaders.Load() == 1 })
	time.Sleep(300 * time.Millisecond)
	if max.Load() != 1 {
		t.Fatal("two leaders at once")
	}
	// Stopping one candidate hands leadership over (if it was the leader) or changes nothing.
	stopA()
	waitFor(t, func() bool { return leaders.Load() == 1 })
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(25 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timeout")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
