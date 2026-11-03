package api

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/rhisijmuiden/remote-support-tool/server/internal/audit"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/auth"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/sessions"
)

// Technicians see their own sessions; administrators see all of them.
func canSee(u *auth.User, s *sessions.Session) bool {
	return u.IsAdmin() || (s.UserID != nil && *s.UserID == u.ID)
}

// loadSession returns the session in the URL if the current user may see it.
func (a *API) loadSession(w http.ResponseWriter, r *http.Request) *sessions.Session {
	id, ok := idParam(w, r)
	if !ok {
		return nil
	}
	s, err := a.Sessions.Get(r.Context(), id)
	if errors.Is(err, sessions.ErrNotFound) || (err == nil && !canSee(auth.FromContext(r.Context()), s)) {
		fail(w, 404, "session_not_found", nil)
		return nil
	}
	if err != nil {
		fail(w, 500, "internal", err)
		return nil
	}
	return s
}

func sessionFilter(r *http.Request) sessions.Filter {
	q := r.URL.Query()
	num := func(k string) int64 { n, _ := strconv.ParseInt(q.Get(k), 10, 64); return n }
	day := func(k string, add int) time.Time {
		t, err := time.ParseInLocation("2006-01-02", q.Get(k), time.Local)
		if err != nil {
			return time.Time{}
		}
		return t.AddDate(0, 0, add)
	}
	f := sessions.Filter{
		DeviceID:   num("device"),
		UserID:     num("user"),
		CustomerID: num("customer"),
		From:       day("from", 0),
		To:         day("to", 1), // inclusive end date
		Query:      q.Get("q"),
		Recorded:   q.Get("recorded") == "1",
		Active:     q.Get("active") == "1",
		Limit:      int(num("limit")),
		Offset:     int(num("offset")),
	}
	if u := auth.FromContext(r.Context()); !u.IsAdmin() {
		f.UserID = u.ID
	}
	return f
}

func (a *API) listSessions(w http.ResponseWriter, r *http.Request) {
	list, total, err := a.Sessions.List(r.Context(), sessionFilter(r))
	if err != nil {
		fail(w, 500, "internal", err)
		return
	}
	pol, _ := a.Sessions.Policy(r.Context())
	writeJSON(w, 200, map[string]any{"sessions": list, "total": total, "recording_mode": pol.Mode})
}

func (a *API) getSession(w http.ResponseWriter, r *http.Request) {
	s := a.loadSession(w, r)
	if s == nil {
		return
	}
	ev, err := a.Sessions.Events(r.Context(), s.ID)
	if err != nil {
		fail(w, 500, "internal", err)
		return
	}
	sh, err := a.Sessions.Shares(r.Context(), s.ID)
	if err != nil {
		fail(w, 500, "internal", err)
		return
	}
	pol, _ := a.Sessions.Policy(r.Context())
	writeJSON(w, 200, map[string]any{"session": s, "events": ev, "shares": sh, "share_days": pol.ShareDays})
}

// csvCell neutralises values a spreadsheet would run as a formula.
func csvCell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

func (a *API) exportSessions(w http.ResponseWriter, r *http.Request) {
	f := sessionFilter(r)
	f.Limit, f.Offset = 1000, 0
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="sessions-%s.csv"`, time.Now().Format("2006-01-02")))
	cw := csv.NewWriter(w)
	cw.Write([]string{"id", "started_at", "ended_at", "duration_seconds", "kind", "transport", "device", "customer", "technician", "technician_email", "viewer_ip", "end_reason", "recording"})
	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	rows := 0
	for rows < 100000 {
		list, _, err := a.Sessions.List(r.Context(), f)
		if err != nil {
			break
		}
		for _, s := range list {
			end, dur := "", ""
			if s.EndedAt != nil {
				end = s.EndedAt.UTC().Format(time.RFC3339)
				dur = strconv.Itoa(int(s.EndedAt.Sub(s.StartedAt).Seconds()))
			}
			rec := []string{strconv.FormatInt(s.ID, 10), s.StartedAt.UTC().Format(time.RFC3339), end, dur, s.Kind, str(s.Transport),
				str(s.DeviceName), str(s.CustomerName), str(s.UserName), str(s.UserEmail), str(s.ViewerIP), str(s.EndReason), s.Recording}
			for i := range rec {
				rec[i] = csvCell(rec[i])
			}
			cw.Write(rec)
		}
		rows += len(list)
		if len(list) < f.Limit {
			break
		}
		f.Offset += f.Limit
	}
	cw.Flush()
	a.Audit.Record(r.Context(), actor(r), "sessions.exported", "", map[string]int{"rows": rows})
}

// ---- recordings ----

var safeName = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

var mimeRe = regexp.MustCompile(`^video/(webm|mp4)(;\s*codecs="?[a-zA-Z0-9.,\s]+"?)?$`)

func baseMIME(m *string) string {
	if m == nil || *m == "" {
		return "video/webm"
	}
	return strings.TrimSpace(strings.SplitN(*m, ";", 2)[0])
}

func recordingName(s *sessions.Session) string {
	dev := "device"
	if s.DeviceName != nil {
		dev = safeName.ReplaceAllString(*s.DeviceName, "-")
	}
	ext := "webm"
	if baseMIME(s.RecMIME) == "video/mp4" {
		ext = "mp4"
	}
	return fmt.Sprintf("session-%d-%s-%s.%s", s.ID, dev, s.StartedAt.Format("2006-01-02"), ext)
}

func (a *API) streamRecording(w http.ResponseWriter, r *http.Request, s *sessions.Session, download bool) {
	rd, err := a.Sessions.OpenRecording(s)
	if errors.Is(err, sessions.ErrNoRecording) {
		fail(w, 404, "no_recording", nil)
		return
	}
	if err != nil {
		fail(w, 500, "internal", err)
		return
	}
	w.Header().Set("Content-Type", baseMIME(s.RecMIME))
	w.Header().Set("Cache-Control", "private, no-store")
	disp := "inline"
	if download {
		disp = "attachment"
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`%s; filename="%s"`, disp, recordingName(s)))
	// ServeContent handles Range requests, so the player can seek.
	http.ServeContent(w, r, "", time.Time{}, rd)
}

// firstRequest is false for the follow-up Range requests a video player makes while
// seeking, so one viewing is logged once.
func firstRequest(r *http.Request) bool {
	rg := r.Header.Get("Range")
	return rg == "" || strings.HasPrefix(rg, "bytes=0-")
}

func (a *API) getRecording(w http.ResponseWriter, r *http.Request) {
	s := a.loadSession(w, r)
	if s == nil {
		return
	}
	dl := r.URL.Query().Get("download") == "1"
	action := "recording.viewed"
	if dl {
		action = "recording.downloaded"
	}
	if firstRequest(r) {
		a.Audit.Record(r.Context(), actor(r), action, sessionTarget(s), nil)
		a.Sessions.AddEvent(r.Context(), s.ID, action, map[string]string{"by": auth.FromContext(r.Context()).Email})
	}
	a.streamRecording(w, r, s, dl)
}

func sessionTarget(s *sessions.Session) string {
	t := "#" + strconv.FormatInt(s.ID, 10)
	if s.DeviceName != nil {
		t += " " + *s.DeviceName
	}
	return t
}

// Only the technician who runs the session uploads its recording.
func (a *API) ownSession(w http.ResponseWriter, r *http.Request) *sessions.Session {
	s := a.loadSession(w, r)
	if s == nil {
		return nil
	}
	if s.UserID == nil || *s.UserID != auth.FromContext(r.Context()).ID {
		fail(w, 403, "forbidden", nil)
		return nil
	}
	return s
}

func (a *API) putChunk(w http.ResponseWriter, r *http.Request) {
	s := a.ownSession(w, r)
	if s == nil {
		return
	}
	seq, err := strconv.Atoi(chi.URLParam(r, "seq"))
	if err != nil || seq < 0 {
		fail(w, 400, "bad_request", err)
		return
	}
	mime := r.Header.Get("Content-Type")
	if !mimeRe.MatchString(mime) {
		fail(w, 400, "bad_request", errors.New("unsupported content type"))
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, sessions.MaxChunk))
	if err != nil {
		fail(w, 413, "chunk_too_large", nil)
		return
	}
	switch err := a.Sessions.AppendChunk(r.Context(), s.ID, seq, mime, body); {
	case errors.Is(err, sessions.ErrNotRecording):
		fail(w, 409, "not_recording", nil)
	case errors.Is(err, sessions.ErrChunkOrder):
		fail(w, 409, "chunk_out_of_order", nil)
	case err != nil:
		fail(w, 500, "internal", err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (a *API) finishRecording(w http.ResponseWriter, r *http.Request) {
	s := a.ownSession(w, r)
	if s == nil {
		return
	}
	if err := a.Sessions.Finish(r.Context(), s.ID); err != nil {
		if errors.Is(err, sessions.ErrNotRecording) {
			fail(w, 409, "not_recording", nil)
			return
		}
		fail(w, 500, "internal", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) deleteRecording(w http.ResponseWriter, r *http.Request) {
	s := a.loadSession(w, r)
	if s == nil {
		return
	}
	if err := a.Sessions.DeleteRecording(r.Context(), s.ID, "deleted by "+auth.FromContext(r.Context()).Email); err != nil {
		fail(w, 500, "internal", err)
		return
	}
	a.Audit.Record(r.Context(), actor(r), "recording.deleted", sessionTarget(s), nil)
	w.WriteHeader(http.StatusNoContent)
}

// ---- share links ----

func (a *API) createShare(w http.ResponseWriter, r *http.Request) {
	s := a.loadSession(w, r)
	if s == nil {
		return
	}
	var in struct {
		Days int    `json:"days"`
		Note string `json:"note"`
	}
	if !decode(w, r, &in) {
		return
	}
	if len(in.Note) > 200 {
		in.Note = in.Note[:200]
	}
	u := auth.FromContext(r.Context())
	tok, sh, err := a.Sessions.CreateShare(r.Context(), s.ID, u.ID, u.Email, in.Note, in.Days)
	switch {
	case errors.Is(err, sessions.ErrShareDays):
		fail(w, 400, "share_days_invalid", nil)
		return
	case errors.Is(err, sessions.ErrNoRecording):
		fail(w, 404, "no_recording", nil)
		return
	case err != nil:
		fail(w, 500, "internal", err)
		return
	}
	a.Audit.Record(r.Context(), actor(r), "recording.shared", sessionTarget(s), map[string]any{"share_id": sh.ID, "expires_at": sh.ExpiresAt})
	url := strings.TrimRight(a.Cfg.PublicURL, "/") + "/r/" + tok
	writeJSON(w, 201, map[string]any{"url": url, "share": sh})
}

func (a *API) revokeShare(w http.ResponseWriter, r *http.Request) {
	s := a.loadSession(w, r)
	if s == nil {
		return
	}
	sid, err := strconv.ParseInt(chi.URLParam(r, "share"), 10, 64)
	if err != nil {
		fail(w, 400, "bad_request", err)
		return
	}
	if err := a.Sessions.RevokeShare(r.Context(), s.ID, sid); err != nil {
		if errors.Is(err, sessions.ErrShareInvalid) {
			fail(w, 404, "share_invalid", nil)
			return
		}
		fail(w, 500, "internal", err)
		return
	}
	a.Audit.Record(r.Context(), actor(r), "recording.share_revoked", sessionTarget(s), map[string]int64{"share_id": sid})
	w.WriteHeader(http.StatusNoContent)
}

// publicRecording is what a customer sees on the share page: no IPs or internal ids.
func (a *API) publicRecording(w http.ResponseWriter, r *http.Request) {
	s, sh, err := a.Sessions.ResolveShare(r.Context(), chi.URLParam(r, "token"), false)
	if err != nil {
		fail(w, 404, "share_invalid", nil)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{
		"company_name":    a.Cfg.Branding.CompanyName,
		"product_name":    a.Cfg.Branding.ProductName,
		"device_name":     s.DeviceName,
		"customer_name":   s.CustomerName,
		"technician_name": s.UserName,
		"started_at":      s.StartedAt,
		"ended_at":        s.EndedAt,
		"size_bytes":      s.RecBytes,
		"complete":        s.Recording == sessions.RecComplete,
		"expires_at":      sh.ExpiresAt,
		"note":            sh.Note,
	})
}

func (a *API) publicVideo(w http.ResponseWriter, r *http.Request) {
	s, sh, err := a.Sessions.ResolveShare(r.Context(), chi.URLParam(r, "token"), firstRequest(r))
	if err != nil {
		fail(w, 404, "share_invalid", nil)
		return
	}
	dl := r.URL.Query().Get("download") == "1"
	action := "recording.share_viewed"
	if dl {
		action = "recording.share_downloaded"
	}
	if firstRequest(r) {
		a.Audit.Record(r.Context(), actorIP(r), action, sessionTarget(s), map[string]int64{"share_id": sh.ID})
		a.Sessions.AddEvent(r.Context(), s.ID, action, map[string]any{"share_id": sh.ID, "ip": clientIP(r)})
	}
	a.streamRecording(w, r, s, dl)
}

func actorIP(r *http.Request) audit.Actor { return audit.Actor{IP: clientIP(r)} }

// ---- settings ----

func (a *API) getRecordingPolicy(w http.ResponseWriter, r *http.Request) {
	p, err := a.Sessions.Policy(r.Context())
	if err != nil {
		fail(w, 500, "internal", err)
		return
	}
	writeJSON(w, 200, p)
}

func (a *API) putRecordingPolicy(w http.ResponseWriter, r *http.Request) {
	var p sessions.Policy
	if !decode(w, r, &p) {
		return
	}
	if err := a.Sessions.SavePolicy(r.Context(), p); err != nil {
		if errors.Is(err, sessions.ErrPolicy) {
			fail(w, 400, "recording_policy_invalid", nil)
			return
		}
		fail(w, 500, "internal", err)
		return
	}
	a.Audit.Record(r.Context(), actor(r), "settings.recording_saved", p.Mode, p)
	w.WriteHeader(http.StatusNoContent)
}
