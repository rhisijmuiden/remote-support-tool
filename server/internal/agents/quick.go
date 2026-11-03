package agents

// QuickSupport: the portable client a customer starts once. It registers
// without an enrollment token, becomes a device of kind "quick" and then uses
// the same control channel and session sockets as the Windows agent. Its
// device code is the code the customer reads out. A technician enters that
// code in the portal; the client asks the customer to allow the session.
//
// When the client closes (or has been offline for QuickRetireAfter), the
// device is retired: its token is removed and its code is freed, while the
// row stays for the session history.

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/rhisijmuiden/remote-support-tool/server/internal/audit"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/auth"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/builder"
)

const (
	// QuickExe is the QuickSupport file name in AgentDir and in release assets
	// (the same name the client builder imports).
	QuickExe = "rhis-quick_windows_amd64.exe"
	// QuickVersionFile in AgentDir holds the version of the Windows client
	// workspace (e.g. "0.2.0"), which QuickSupport shares.
	QuickVersionFile = "client.version"
	// QuickRetireAfter frees the code of a quick device that has been offline this long.
	QuickRetireAfter = 3 * time.Minute
)

var ErrInvalidCode = errors.New("invalid_code")

// QuickOptions is what the server writes into each downloaded QuickSupport exe.
type QuickOptions struct {
	PublicURL   string // https://remote.example.com
	ProductName string
	CompanyName string
	// UpdateKeys are the public update keys (base64 Ed25519) the client trusts
	// for signed updates; without them a download cannot be built.
	UpdateKeys func() []string
	// Channel returns the server's release channel ("production" or "test").
	Channel func(ctx context.Context) string
}

// QuickRegisterRequest is sent by the QuickSupport client when it starts.
type QuickRegisterRequest struct {
	MachineID     string `json:"machine_id"`
	Hostname      string `json:"hostname"`
	OSVersion     string `json:"os_version"`
	User          string `json:"user"`
	ClientVersion string `json:"client_version"`
	Language      string `json:"language"`
}

var (
	quickRegisterRL rateLimiter // registrations per IP
	quickLookupRL   rateLimiter // failed code lookups per technician
)

// NormalizeCode accepts "123 456 789", "123-456-789" or "123456789".
func NormalizeCode(in string) (string, error) {
	var b strings.Builder
	for _, r := range in {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '.':
		default:
			return "", ErrInvalidCode
		}
	}
	if b.Len() != 9 {
		return "", ErrInvalidCode
	}
	return b.String(), nil
}

// QuickRegister creates a quick device and returns its code and token.
func (s *Service) QuickRegister(ctx context.Context, in QuickRegisterRequest, ip string) (*EnrollResponse, error) {
	in.Hostname = clean(in.Hostname, 100)
	if in.Hostname == "" {
		return nil, errors.New("invalid_input")
	}
	tok := newToken()
	out := &EnrollResponse{DeviceToken: tok}
	var err error
	for range 5 {
		var code string
		if code, err = NewDeviceCode(ctx, s.DB); err != nil {
			return nil, err
		}
		err = s.DB.QueryRow(ctx, `INSERT INTO devices(kind, machine_id, token_hash, device_code, hostname, os_version, agent_version, last_user, public_ip, enrolled_at)
			VALUES('quick',$1,$2,$3,$4,$5,$6,$7,$8,now()) RETURNING id, device_code`,
			clean(in.MachineID, 100), hashToken(tok), code, in.Hostname, clean(in.OSVersion, 200), clean(in.ClientVersion, 50),
			nullIfEmpty(clean(in.User, 200)), ip).Scan(&out.DeviceID, &out.DeviceCode)
		if !isUniqueViolation(err) {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	s.Audit.Record(ctx, audit.Actor{IP: ip}, "quick.registered", in.Hostname, map[string]any{"device_id": out.DeviceID})
	return out, nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// QuickRetire removes a quick device's token and frees its code.
func (s *Service) QuickRetire(ctx context.Context, deviceID int64, reason string) error {
	var host string
	err := s.DB.QueryRow(ctx, `UPDATE devices SET token_hash=NULL, online=false, device_code='q'||id::text
		WHERE id=$1 AND kind='quick' AND token_hash IS NOT NULL RETURNING hostname`, deviceID).Scan(&host)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	s.endDeviceSessions(deviceID, "quick_closed")
	s.Disconnect(deviceID, "retired")
	s.Audit.Record(ctx, audit.Actor{}, "quick.closed", host, map[string]any{"device_id": deviceID, "reason": reason})
	return nil
}

// QuickJanitor retires quick devices that have been offline for QuickRetireAfter.
// It runs until ctx ends; with several servers each runs it, which is harmless.
func (s *Service) QuickJanitor(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		s.quickSweep(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Service) quickSweep(ctx context.Context) {
	cut := time.Now().Add(-QuickRetireAfter)
	rows, err := s.DB.Query(ctx, `SELECT id FROM devices WHERE kind='quick' AND token_hash IS NOT NULL AND NOT online
		AND COALESCE(last_seen_at, enrolled_at) < $1`, cut)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("quick sweep", "err", err)
		}
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		if s.Online(id) {
			continue // connected to this server; the database flag lags behind
		}
		if err := s.QuickRetire(ctx, id, "offline"); err != nil {
			slog.Error("quick retire", "device", id, "err", err)
		}
	}
}

// QuickConnect starts a quick session for the device with this code.
func (s *Service) QuickConnect(ctx context.Context, code string, u *auth.User, viewerIP string) (*StartResult, error) {
	code, err := NormalizeCode(code)
	if err != nil {
		return nil, err
	}
	var id int64
	err = s.DB.QueryRow(ctx, `SELECT id FROM devices WHERE kind='quick' AND device_code=$1 AND token_hash IS NOT NULL`, code).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.StartSession(ctx, id, u, "quick", false, viewerIP)
}

// ---- HTTP ----

// quickAgentRoutes adds the client-side QuickSupport routes to /api/agent/v1.
func (s *Service) quickAgentRoutes(r chi.Router) {
	r.Post("/quick/register", s.handleQuickRegister)
	r.Post("/quick/close", s.handleQuickClose)
	r.Get("/quick/latest", s.handleQuickLatest)
	r.Get("/quick/download", s.handleQuickDownload)
}

// quickPortalRoutes adds the technician-side routes to the portal API.
func (s *Service) quickPortalRoutes(r chi.Router) {
	r.Post("/quick/connect", s.handleQuickConnect)
	r.Get("/quick/info", s.handleQuickInfo)
}

func (s *Service) handleQuickRegister(w http.ResponseWriter, r *http.Request) {
	ip := remoteIP(r)
	if quickRegisterRL.blocked(ip, 30, 15*time.Minute) {
		fail(w, http.StatusTooManyRequests, "too_many_attempts", nil)
		return
	}
	quickRegisterRL.add(ip)
	var in QuickRegisterRequest
	if !decode(w, r, &in) {
		return
	}
	out, err := s.QuickRegister(r.Context(), in, ip)
	switch {
	case err != nil && err.Error() == "invalid_input":
		fail(w, 400, "invalid_input", nil)
	case err != nil:
		fail(w, 500, "internal", err)
	default:
		writeJSON(w, 200, out)
	}
}

func (s *Service) handleQuickClose(w http.ResponseWriter, r *http.Request) {
	d, err := s.deviceForToken(r.Context(), bearer(r))
	if err != nil || d.Kind != "quick" {
		fail(w, 401, "unauthorized", nil)
		return
	}
	if err := s.QuickRetire(r.Context(), d.ID, "client_closed"); err != nil {
		fail(w, 500, "internal", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleQuickConnect(w http.ResponseWriter, r *http.Request) {
	u := auth.FromContext(r.Context())
	key := fmt.Sprint(u.ID)
	if quickLookupRL.blocked(key, 20, 15*time.Minute) {
		fail(w, http.StatusTooManyRequests, "quick_too_many", nil)
		return
	}
	var in struct {
		Code string `json:"code"`
	}
	if !decode(w, r, &in) {
		return
	}
	res, err := s.QuickConnect(r.Context(), in.Code, u, remoteIP(r))
	switch {
	case errors.Is(err, ErrInvalidCode):
		fail(w, 400, "invalid_code", nil)
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrOffline):
		// Same answer for unknown and offline codes, and both count against the limit.
		quickLookupRL.add(key)
		s.Audit.Record(r.Context(), actorOf(r), "quick.code_not_found", "", nil)
		fail(w, 404, "quick_not_found", nil)
	case err != nil:
		fail(w, 500, "internal", err)
	default:
		writeJSON(w, 200, res)
	}
}

type quickFile struct {
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
	modTime time.Time
	path    string
}

var quickCache struct {
	sync.Mutex
	f quickFile
}

// quickFile returns the QuickSupport exe on disk with its version and checksum, or nil.
func (s *Service) quickFile() *quickFile {
	if s.AgentDir == "" {
		return nil
	}
	p := filepath.Join(s.AgentDir, QuickExe)
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() {
		return nil
	}
	quickCache.Lock()
	defer quickCache.Unlock()
	if c := quickCache.f; c.path == p && c.modTime.Equal(fi.ModTime()) && c.Size == fi.Size() {
		return &c
	}
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil
	}
	v, _ := os.ReadFile(filepath.Join(s.AgentDir, QuickVersionFile))
	quickCache.f = quickFile{Version: strings.TrimSpace(string(v)), SHA256: hex.EncodeToString(h.Sum(nil)), Size: fi.Size(), modTime: fi.ModTime(), path: p}
	c := quickCache.f
	return &c
}

// quickConfig is the configuration written into a downloaded exe, in the
// client builder layout (docs/client-builder.md). The client reads it in
// client/quicksupport/src/config.rs.
func (s *Service) quickConfig(ctx context.Context) (*builder.ClientConfig, error) {
	cfg := &builder.ClientConfig{V: 1, Mode: "quick", ProductName: s.Quick.ProductName, CompanyName: s.Quick.CompanyName,
		Servers: []string{strings.TrimRight(s.Quick.PublicURL, "/")}, Channel: "production"}
	if s.Quick.Channel != nil {
		cfg.Channel = s.Quick.Channel(ctx)
	}
	if s.Quick.UpdateKeys != nil {
		cfg.UpdateKeys = s.Quick.UpdateKeys()
	}
	if cfg.ProductName == "" {
		cfg.ProductName = "RHIS Remote"
	}
	servers, err := builder.NormalizeServers(cfg.Servers)
	if err != nil {
		return nil, err
	}
	cfg.Servers = servers
	return cfg, cfg.Validate()
}

var quickBuilt struct {
	sync.Mutex
	key string
	exe []byte
	sum string
}

// quickDownload returns the exe with this server's configuration embedded,
// cached until the file or the configuration changes.
func (s *Service) quickDownload(ctx context.Context, qf *quickFile) ([]byte, string, error) {
	cfg, err := s.quickConfig(ctx)
	if err != nil {
		return nil, "", err
	}
	js, _ := json.Marshal(cfg)
	key := qf.path + "|" + qf.SHA256 + "|" + string(js)
	quickBuilt.Lock()
	defer quickBuilt.Unlock()
	if quickBuilt.key == key {
		return quickBuilt.exe, quickBuilt.sum, nil
	}
	raw, err := os.ReadFile(qf.path)
	if err != nil {
		return nil, "", err
	}
	exe, err := embedQuick(raw, cfg)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(exe)
	quickBuilt.key, quickBuilt.exe, quickBuilt.sum = key, exe, hex.EncodeToString(sum[:])
	return exe, quickBuilt.sum, nil
}

// embedQuick writes cfg into exe. An unsigned exe gets the builder's trailer
// (builder.Embed). A signed one keeps its signature: the same trailer goes after
// the certificate table, outside what the signature covers, where the client
// also looks. Windows accepts that unless the optional EnableCertPaddingCheck
// policy is on; a build from the client builder is re-signed instead.
func embedQuick(exe []byte, cfg *builder.ClientConfig) ([]byte, error) {
	signed, err := builder.IsSigned(exe)
	if err != nil {
		return nil, err
	}
	if !signed {
		return builder.Embed(exe, cfg)
	}
	if _, err := builder.Extract(exe); err == nil {
		return nil, errors.New("executable already contains a configuration")
	}
	js, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	pad := (8 - (len(exe)+len(js)+12)%8) % 8
	out := make([]byte, 0, len(exe)+pad+len(js)+12)
	out = append(out, exe...)
	out = append(out, make([]byte, pad)...)
	out = append(out, js...)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(js)))
	return append(out, builder.Magic...), nil
}

// handleQuickLatest tells a running client which version the server offers.
// It is for clients without update keys; downloads from this server carry the
// keys and use the signed update endpoint. The checksum covers the exe as downloaded.
func (s *Service) handleQuickLatest(w http.ResponseWriter, r *http.Request) {
	qf := s.quickFile()
	if qf == nil || qf.Version == "" {
		fail(w, 404, "quick_not_available", nil)
		return
	}
	_, sum, err := s.quickDownload(r.Context(), qf)
	if err != nil {
		fail(w, 503, "quick_not_available", err)
		return
	}
	writeJSON(w, 200, map[string]any{"version": qf.Version, "url": "/api/agent/v1/quick/download", "sha256": sum})
}

// handleQuickDownload serves the exe with this server's address and update keys
// embedded, so the customer only has to start it. It is public: the exe holds no
// secrets. Embedding removes a signature, so a signed exe should come from the
// client builder instead.
func (s *Service) handleQuickDownload(w http.ResponseWriter, r *http.Request) {
	qf := s.quickFile()
	if qf == nil {
		fail(w, 404, "quick_not_available", nil)
		return
	}
	exe, _, err := s.quickDownload(r.Context(), qf)
	if err != nil {
		fail(w, 503, "quick_not_available", err)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.microsoft.portable-executable")
	w.Header().Set("Content-Disposition", `attachment; filename="RHIS-QuickSupport.exe"`)
	w.Header().Set("Content-Length", fmt.Sprint(len(exe)))
	w.Header().Set("Cache-Control", "no-cache")
	if r.Method == http.MethodHead {
		return
	}
	w.Write(exe)
}

// handleQuickInfo tells the portal whether the QuickSupport download exists.
func (s *Service) handleQuickInfo(w http.ResponseWriter, r *http.Request) {
	qf := s.quickFile()
	out := map[string]any{"available": qf != nil, "download_path": "/api/agent/v1/quick/download",
		"download_url": strings.TrimRight(s.Quick.PublicURL, "/") + "/api/agent/v1/quick/download",
		"https":        strings.HasPrefix(s.Quick.PublicURL, "https://")}
	if qf != nil {
		out["version"] = qf.Version
		out["size"] = qf.Size
	}
	writeJSON(w, 200, out)
}
