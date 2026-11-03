package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rhisijmuiden/remote-support-tool/server/internal/audit"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/auth"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/db"
)

// testDB uses its own database, so it can run in parallel with other packages' database tests
// (the backup tests run pg_dump on the shared test database).
func testDB(t *testing.T) *pgxpool.Pool {
	raw := os.Getenv("RHIS_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("RHIS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := db.Open(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	const name = "rhis_agents_test"
	var exists bool
	admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)`, name).Scan(&exists)
	if !exists {
		if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
			t.Fatal(err)
		}
	}
	admin.Close()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	pool, err := db.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func postJSON(t *testing.T, url string, body any, out any) int {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func TestEnrollControlAndSession(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	var uid int64
	if err := pool.QueryRow(ctx, `INSERT INTO users(email,name,password_hash,role) VALUES('t@example.com','Tess','x','technician') RETURNING id`).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	user := &auth.User{ID: uid, Email: "t@example.com", Name: "Tess", Role: "technician"}
	svc := &Service{DB: pool, Audit: &audit.Log{DB: pool}, PingInterval: time.Second, ICE: StaticICE{{URLs: []string{"stun:stun.example.com:3478"}}}}
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/api/agent/v1/", http.StripPrefix("/api/agent/v1", svc.AgentRoutes()))
	portal := chi.NewRouter()
	portal.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.WithUser(r.Context(), user)))
		})
	})
	svc.PortalRoutes(portal)
	mux.Handle("/api/", http.StripPrefix("/api", portal))
	srv := httptest.NewServer(mux)
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	// Bad token is refused.
	if code := postJSON(t, srv.URL+"/api/agent/v1/enroll", EnrollRequest{EnrollToken: "nope", MachineID: "m1", Hostname: "PC-01"}, nil); code != 403 {
		t.Fatalf("bad token: %d", code)
	}
	one := 1
	tok, err := svc.CreateToken(ctx, uid, "test", nil, 7, &one)
	if err != nil {
		t.Fatal(err)
	}
	var en EnrollResponse
	req := EnrollRequest{EnrollToken: tok.Token, MachineID: "m1", Hostname: "PC-01", OSVersion: "Windows 11", AgentVersion: "0.2.0"}
	if code := postJSON(t, srv.URL+"/api/agent/v1/enroll", req, &en); code != 200 || en.DeviceToken == "" || len(en.DeviceCode) != 9 {
		t.Fatalf("enroll: %d %+v", code, en)
	}
	// The token had one use.
	if code := postJSON(t, srv.URL+"/api/agent/v1/enroll", req, nil); code != 403 {
		t.Fatalf("token reuse: %d", code)
	}

	hdr := http.Header{"Authorization": {"Bearer " + en.DeviceToken}}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, _, err := websocket.Dial(cctx, wsURL+"/api/agent/v1/control", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer wrong"}}}); err == nil {
		t.Fatal("control accepted a wrong token")
	}
	ctl, _, err := websocket.Dial(cctx, wsURL+"/api/agent/v1/control", &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.CloseNow()
	wsjson.Write(cctx, ctl, map[string]string{"type": "hello", "hostname": "PC-01", "user": "ACME\\jan", "agent_version": "0.2.1"})
	var m map[string]any
	if err := wsjson.Read(cctx, ctl, &m); err != nil || m["type"] != "welcome" {
		t.Fatalf("welcome: %v %v", m, err)
	}
	var online bool
	var lastUser string
	pool.QueryRow(ctx, `SELECT online, last_user FROM devices WHERE id=$1`, en.DeviceID).Scan(&online, &lastUser)
	if !online || lastUser != `ACME\jan` {
		t.Fatalf("presence: online=%v user=%q", online, lastUser)
	}

	res, err := svc.StartSession(ctx, en.DeviceID, user, "unattended", true, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	for {
		if err := wsjson.Read(cctx, ctl, &m); err != nil {
			t.Fatal(err)
		}
		if m["type"] == "session_request" {
			break
		}
	}
	if m["session_id"] != res.SessionID || m["recording"] != true || m["user_name"] != "Tess" {
		t.Fatalf("session_request: %v", m)
	}
	wsjson.Write(cctx, ctl, map[string]string{"type": "session_accept", "session_id": res.SessionID})

	ag, _, err := websocket.Dial(cctx, wsURL+"/api/agent/v1/sessions/"+res.SessionID, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatal(err)
	}
	defer ag.CloseNow()
	vw, _, err := websocket.Dial(cctx, wsURL+"/api/sessions/"+res.SessionID+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer vw.CloseNow()
	expect := func(c *websocket.Conn, typ string) map[string]any {
		t.Helper()
		var v map[string]any
		if err := wsjson.Read(cctx, c, &v); err != nil {
			t.Fatalf("read %s: %v", typ, err)
		}
		if v["type"] != typ {
			t.Fatalf("want %s, got %v", typ, v)
		}
		return v
	}
	expect(ag, "ready")
	expect(vw, "ready")
	wsjson.Write(cctx, vw, map[string]string{"type": "offer", "sdp": "v=0"})
	if v := expect(ag, "offer"); v["sdp"] != "v=0" {
		t.Fatal(v)
	}
	wsjson.Write(cctx, ag, map[string]string{"type": "answer", "sdp": "v=0 answer"})
	expect(vw, "answer")
	// Relay frames pass through unchanged.
	key := append([]byte{1, 0, 0, 0, 0, 0, 0, 0, 1}, []byte("h264")...)
	ag.Write(cctx, websocket.MessageBinary, key)
	typ, data, err := vw.Read(cctx)
	if err != nil || typ != websocket.MessageBinary || !bytes.Equal(data, key) {
		t.Fatalf("relay frame: %v %v", data, err)
	}
	logID := svc.session(res.SessionID).LogID
	wsjson.Write(cctx, vw, map[string]string{"type": "transport", "transport": "websocket"})
	wsjson.Write(cctx, vw, map[string]string{"type": "end", "reason": "user"})
	expect(ag, "end")

	var transport, reason string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pool.QueryRow(ctx, `SELECT COALESCE(transport,''), COALESCE(end_reason,'') FROM remote_sessions WHERE id=$1`, logID).Scan(&transport, &reason)
		if reason != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if transport != "websocket" || reason != "viewer:user" {
		t.Fatalf("session log: transport=%q reason=%q", transport, reason)
	}

	// Disconnecting the agent marks it offline.
	ctl.Close(websocket.StatusNormalClosure, "bye")
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && svc.Online(en.DeviceID) {
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	pool.QueryRow(ctx, `SELECT online FROM devices WHERE id=$1`, en.DeviceID).Scan(&online)
	if online {
		t.Fatal("device still online")
	}
	if _, err := svc.StartSession(ctx, en.DeviceID, user, "unattended", false, ""); err != ErrOffline {
		t.Fatalf("offline start: %v", err)
	}
}

func TestRemoteIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:5000"
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 198.51.100.4")
	if got := remoteIP(r); got != "198.51.100.4" {
		t.Fatal(got)
	}
	r.RemoteAddr = "198.51.100.7:5000"
	if got := remoteIP(r); got != "198.51.100.7" {
		t.Fatal(got)
	}
}
