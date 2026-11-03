package agents

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	"github.com/rhisijmuiden/remote-support-tool/server/internal/audit"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/auth"
)

// AgentExe is the file name of the Windows agent in AgentDir and in release assets.
const AgentExe = "rhis-agent_windows_amd64.exe"

// MountAgentRoutes adds the agent routes to the router served at /api/agent/v1. Agents
// authenticate with a bearer device token, so these routes sit outside the portal's cookie
// and CSRF checks. Other packages (updates, quick support) mount on the same router.
func (s *Service) MountAgentRoutes(r chi.Router) {
	s.mu.Lock()
	s.init()
	s.mu.Unlock()
	r.Post("/enroll", s.handleEnroll)
	r.Get("/control", s.handleControl)
	r.Get("/sessions/{id}", s.handleAgentSession)
	r.Get("/download/windows", s.handleDownload)
}

// PortalRoutes adds the routes for signed-in portal users to r.
func (s *Service) PortalRoutes(r chi.Router) {
	r.Post("/devices/{id}/sessions", s.handleStart)
	r.Delete("/devices/{id}", s.handleDeleteDevice)
	r.Get("/live/{id}", s.handleSessionInfo)
	r.Get("/live/{id}/ws", s.handleViewer)
	r.Get("/enrollment-tokens", s.handleListTokens)
	r.Post("/enrollment-tokens", s.handleCreateToken)
	r.Delete("/enrollment-tokens/{id}", s.handleRevokeToken)
	r.Get("/agent", s.handleAgentInfo)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, key string, err error) {
	out := map[string]string{"error": key}
	if err != nil {
		out["detail"] = err.Error()
		if code >= 500 {
			slog.Error("request failed", "error", key, "err", err)
		}
	}
	writeJSON(w, code, out)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		fail(w, http.StatusBadRequest, "bad_request", err)
		return false
	}
	return true
}

func idParam(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	return id, err == nil && id > 0
}

func actorOf(r *http.Request) audit.Actor {
	a := audit.Actor{IP: remoteIP(r)}
	if u := auth.FromContext(r.Context()); u != nil {
		a.UserID, a.Email = u.ID, u.Email
	}
	return a
}

func (s *Service) handleStart(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r)
	if !ok {
		fail(w, 400, "bad_request", nil)
		return
	}
	var in struct {
		Record bool `json:"record"`
	}
	if r.ContentLength != 0 && !decode(w, r, &in) {
		return
	}
	res, err := s.StartSession(r.Context(), id, auth.FromContext(r.Context()), "unattended", in.Record, remoteIP(r))
	switch {
	case errors.Is(err, ErrNotFound):
		fail(w, 404, "not_found", nil)
	case errors.Is(err, ErrOffline):
		fail(w, 409, "device_offline", nil)
	case err != nil:
		fail(w, 500, "internal", err)
	default:
		writeJSON(w, 200, res)
	}
}

// viewerSession returns the session if the signed-in user may view it.
func (s *Service) viewerSession(r *http.Request) *session {
	ss := s.session(chi.URLParam(r, "id"))
	u := auth.FromContext(r.Context())
	if ss == nil || u == nil || ss.UserID != u.ID {
		return nil
	}
	return ss
}

func (s *Service) handleSessionInfo(w http.ResponseWriter, r *http.Request) {
	ss := s.viewerSession(r)
	if ss == nil {
		fail(w, 404, "session_not_found", nil)
		return
	}
	ice, err := s.ICE.ICEServers(r.Context(), auth.FromContext(r.Context()).Email)
	if err != nil {
		fail(w, 500, "internal", err)
		return
	}
	writeJSON(w, 200, SessionInfo{
		SessionID: ss.ID, DeviceID: ss.DeviceID, Hostname: ss.Hostname, DeviceCode: ss.DeviceCode,
		Kind: ss.Kind, Recording: ss.Record, LogID: ss.LogID, ICEServers: ice,
	})
}

func (s *Service) handleViewer(w http.ResponseWriter, r *http.Request) {
	ss := s.viewerSession(r)
	if ss == nil {
		fail(w, 404, "session_not_found", nil)
		return
	}
	// Accept checks that the Origin matches the Host, which stops cross-site WebSocket hijacking.
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	s.join(ss, sideViewer, ws)
}

func (s *Service) handleAgentSession(w http.ResponseWriter, r *http.Request) {
	d, err := s.deviceForToken(r.Context(), bearer(r))
	if err != nil {
		fail(w, 401, "unauthorized", nil)
		return
	}
	ss := s.session(chi.URLParam(r, "id"))
	if ss == nil || ss.DeviceID != d.ID {
		fail(w, 404, "session_not_found", nil)
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	s.join(ss, sideAgent, ws)
}

func (s *Service) handleDeleteDevice(w http.ResponseWriter, r *http.Request) {
	u := auth.FromContext(r.Context())
	if !u.IsAdmin() {
		fail(w, 403, "forbidden", nil)
		return
	}
	id, ok := idParam(r)
	if !ok {
		fail(w, 400, "bad_request", nil)
		return
	}
	var host string
	if err := s.DB.QueryRow(r.Context(), `DELETE FROM devices WHERE id=$1 RETURNING hostname`, id).Scan(&host); err != nil {
		fail(w, 404, "not_found", nil)
		return
	}
	s.endDeviceSessions(id, "device_deleted")
	s.Disconnect(id, "deleted")
	s.Audit.Record(r.Context(), actorOf(r), "device.deleted", host, map[string]any{"device_id": id})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleListTokens(w http.ResponseWriter, r *http.Request) {
	ts, err := s.ListTokens(r.Context())
	if err != nil {
		fail(w, 500, "internal", err)
		return
	}
	writeJSON(w, 200, ts)
}

func (s *Service) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Label      string `json:"label"`
		CustomerID *int64 `json:"customer_id"`
		ValidDays  int    `json:"valid_days"`
		MaxUses    *int   `json:"max_uses"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.ValidDays < 0 || in.ValidDays > 3650 || (in.MaxUses != nil && *in.MaxUses < 1) {
		fail(w, 400, "invalid_input", nil)
		return
	}
	t, err := s.CreateToken(r.Context(), auth.FromContext(r.Context()).ID, in.Label, in.CustomerID, in.ValidDays, in.MaxUses)
	if err != nil {
		fail(w, 500, "internal", err)
		return
	}
	s.Audit.Record(r.Context(), actorOf(r), "enroll_token.created", t.Label, map[string]any{"id": t.ID, "valid_days": in.ValidDays, "max_uses": in.MaxUses})
	writeJSON(w, 200, t)
}

func (s *Service) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r)
	if !ok {
		fail(w, 400, "bad_request", nil)
		return
	}
	if err := s.RevokeToken(r.Context(), id); err != nil {
		if errors.Is(err, ErrNotFound) {
			fail(w, 404, "not_found", nil)
		} else {
			fail(w, 500, "internal", err)
		}
		return
	}
	s.Audit.Record(r.Context(), actorOf(r), "enroll_token.revoked", strconv.FormatInt(id, 10), nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) agentFile() (os.FileInfo, string) {
	if s.AgentDir == "" {
		return nil, ""
	}
	p := filepath.Join(s.AgentDir, AgentExe)
	fi, err := os.Stat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, ""
	}
	return fi, p
}

func (s *Service) handleAgentInfo(w http.ResponseWriter, r *http.Request) {
	fi, _ := s.agentFile()
	out := map[string]any{"available": fi != nil, "download_path": "/api/agent/v1/download/windows"}
	if fi != nil {
		out["size"] = fi.Size()
		out["modified"] = fi.ModTime().UTC().Format(time.RFC3339)
	}
	writeJSON(w, 200, out)
}

func (s *Service) handleDownload(w http.ResponseWriter, r *http.Request) {
	fi, p := s.agentFile()
	if fi == nil {
		fail(w, 404, "agent_not_available", nil)
		return
	}
	f, err := os.Open(p)
	if err != nil {
		fail(w, 500, "internal", err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/vnd.microsoft.portable-executable")
	w.Header().Set("Content-Disposition", `attachment; filename="rhis-agent.exe"`)
	http.ServeContent(w, r, "rhis-agent.exe", fi.ModTime(), f)
}
