package agents

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/go-chi/chi/v5"

	"github.com/rhisijmuiden/remote-support-tool/server/internal/audit"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/auth"
)

func TestNormalizeCode(t *testing.T) {
	for in, want := range map[string]string{"123 456 789": "123456789", "123-456-789": "123456789", " 123456789 ": "123456789"} {
		if got, err := NormalizeCode(in); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, in := range []string{"12345678", "1234567890", "12345678a", ""} {
		if _, err := NormalizeCode(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestQuickSupport(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	var uid int64
	if err := pool.QueryRow(ctx, `INSERT INTO users(email,name,password_hash,role) VALUES('q@example.com','Quinn','x','technician') RETURNING id`).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	user := &auth.User{ID: uid, Email: "q@example.com", Name: "Quinn", Role: "technician"}
	dir := t.TempDir()
	exe := []byte("MZ pretend this is a Windows exe")
	os.WriteFile(filepath.Join(dir, QuickExe), exe, 0o644)
	os.WriteFile(filepath.Join(dir, QuickVersionFile), []byte("0.1.0\n"), 0o644)
	svc := &Service{DB: pool, Audit: &audit.Log{DB: pool}, PingInterval: time.Second, AgentDir: dir,
		Quick: QuickOptions{PublicURL: "https://remote.example.com/", ProductName: "RHIS Remote", CompanyName: "RHIS"}}
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

	// Register without any enrollment token.
	var reg EnrollResponse
	if code := postJSON(t, srv.URL+"/api/agent/v1/quick/register", QuickRegisterRequest{MachineID: "r1", Hostname: "LAPTOP-7", OSVersion: "Windows 11", User: "anna", ClientVersion: "0.1.0"}, &reg); code != 200 || len(reg.DeviceCode) != 9 || reg.DeviceToken == "" {
		t.Fatalf("register: %d %+v", code, reg)
	}
	if code := postJSON(t, srv.URL+"/api/agent/v1/quick/register", QuickRegisterRequest{}, nil); code != 400 {
		t.Fatalf("empty register: %d", code)
	}
	// Quick devices stay out of the device list and dashboard (kind='agent' filters elsewhere).
	var kind string
	pool.QueryRow(ctx, `SELECT kind FROM devices WHERE id=$1`, reg.DeviceID).Scan(&kind)
	if kind != "quick" {
		t.Fatalf("kind %q", kind)
	}

	spaced := reg.DeviceCode[:3] + " " + reg.DeviceCode[3:6] + " " + reg.DeviceCode[6:]
	// Offline: the code is not usable yet, and the answer does not reveal that it exists.
	if code := postJSON(t, srv.URL+"/api/quick/connect", map[string]string{"code": spaced}, nil); code != 404 {
		t.Fatalf("connect offline: %d", code)
	}

	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	ctl, _, err := websocket.Dial(cctx, wsURL+"/api/agent/v1/control", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + reg.DeviceToken}}})
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.CloseNow()
	wsjson.Write(cctx, ctl, map[string]string{"type": "hello", "hostname": "LAPTOP-7", "agent_version": "0.1.0", "machine_id": "r1"})
	var m map[string]any
	if err := wsjson.Read(cctx, ctl, &m); err != nil || m["type"] != "welcome" || m["device_code"] != reg.DeviceCode {
		t.Fatalf("welcome: %v %v", m, err)
	}
	for !svc.Online(reg.DeviceID) {
		time.Sleep(10 * time.Millisecond)
	}

	if code := postJSON(t, srv.URL+"/api/quick/connect", map[string]string{"code": "12ab"}, nil); code != 400 {
		t.Fatalf("bad code: %d", code)
	}
	var start StartResult
	if code := postJSON(t, srv.URL+"/api/quick/connect", map[string]string{"code": spaced}, &start); code != 200 || start.SessionID == "" {
		t.Fatalf("connect: %d %+v", code, start)
	}
	for {
		m = nil
		if err := wsjson.Read(cctx, ctl, &m); err != nil {
			t.Fatal(err)
		}
		if m["type"] == "session_request" {
			break
		}
	}
	if m["kind"] != "quick" || m["user_name"] != "Quinn" || m["session_id"] != start.SessionID {
		t.Fatalf("request: %v", m)
	}

	// Download: the exe with this server's address appended.
	resp, err := http.Get(srv.URL + "/api/agent/v1/quick/download")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.HasPrefix(body, exe) || !bytes.HasSuffix(body, []byte(quickTrailerMagic)) {
		t.Fatalf("download: %d %q", resp.StatusCode, body)
	}
	n := binary.LittleEndian.Uint32(body[len(body)-12:])
	var cfg struct {
		Servers     []string `json:"servers"`
		CompanyName string   `json:"company_name"`
		Mode        string   `json:"mode"`
	}
	if err := json.Unmarshal(body[len(body)-12-int(n):len(body)-12], &cfg); err != nil || len(cfg.Servers) != 1 || cfg.Servers[0] != "https://remote.example.com" || cfg.CompanyName != "RHIS" || cfg.Mode != "quick" {
		t.Fatalf("trailer: %+v %v", cfg, err)
	}
	if len(body)%8 != 0 {
		t.Fatalf("trailer does not end on an 8-byte boundary: %d", len(body))
	}
	var latest struct{ Version, URL, SHA256 string }
	resp, _ = http.Get(srv.URL + "/api/agent/v1/quick/latest")
	json.NewDecoder(resp.Body).Decode(&latest)
	resp.Body.Close()
	sum := sha256.Sum256(body)
	if latest.Version != "0.1.0" || latest.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("latest: %+v", latest)
	}

	// Closing the client frees the code at once.
	req, _ := http.NewRequest("POST", srv.URL+"/api/agent/v1/quick/close", nil)
	req.Header.Set("Authorization", "Bearer "+reg.DeviceToken)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 204 {
		t.Fatalf("close: %v %v", resp, err)
	}
	if code := postJSON(t, srv.URL+"/api/quick/connect", map[string]string{"code": reg.DeviceCode}, nil); code != 404 {
		t.Fatalf("connect after close: %d", code)
	}
	var stored string
	pool.QueryRow(ctx, `SELECT device_code FROM devices WHERE id=$1`, reg.DeviceID).Scan(&stored)
	if stored == reg.DeviceCode {
		t.Fatal("code not freed")
	}
	if _, _, err := websocket.Dial(cctx, wsURL+"/api/agent/v1/control", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + reg.DeviceToken}}}); err == nil {
		t.Fatal("retired token still accepted")
	}

	// A client that never connects is retired by the janitor.
	var reg2 EnrollResponse
	postJSON(t, srv.URL+"/api/agent/v1/quick/register", QuickRegisterRequest{Hostname: "PC-2"}, &reg2)
	pool.Exec(ctx, `UPDATE devices SET enrolled_at=now()-interval '1 hour' WHERE id=$1`, reg2.DeviceID)
	svc.quickSweep(ctx)
	var tokenGone bool
	pool.QueryRow(ctx, `SELECT token_hash IS NULL FROM devices WHERE id=$1`, reg2.DeviceID).Scan(&tokenGone)
	if !tokenGone {
		t.Fatal("janitor did not retire the idle quick device")
	}
}
