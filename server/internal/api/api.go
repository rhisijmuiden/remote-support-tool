// Package api implements the JSON API used by the portal.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rhisijmuiden/remote-support-tool/server/internal/agents"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/agentupdate"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/audit"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/auth"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/backup"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/builder"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/cluster"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/codesign"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/config"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/mail"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/sessions"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/update"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/version"
)

type API struct {
	Cfg       *config.Config
	DB        *pgxpool.Pool
	Auth      *auth.Service
	Audit     *audit.Log
	Mail      *mail.Service
	Backup    *backup.Service
	Scheduler *backup.Scheduler
	Update    *update.Service
	Builder   *builder.Service
	Agents    *agentupdate.Service
	Gateway   *agents.Service // nil when this server does not run the gateway role
	CodeSign  *codesign.Service
	Sessions  *sessions.Service
	Cluster   *cluster.Registry
	Bus       cluster.Bus
	// Restart is called after an update has been installed.
	Restart func()

	limiter loginLimiter
	restore struct {
		sync.Mutex
		Running bool   `json:"running"`
		Name    string `json:"name"`
		Error   string `json:"error"`
		Done    bool   `json:"done"`
	}
}

// Routes returns the /api router.
func (a *API) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, trustedProxyIP(a.Cfg.IsTrustedProxy), middleware.Recoverer, csrf)
	r.Get("/meta", a.meta)
	r.Post("/setup", a.setup)
	r.Post("/login", a.login)
	r.Post("/logout", a.logout)
	r.Get("/public/recordings/{token}", a.publicRecording)
	r.Get("/public/recordings/{token}/video", a.publicVideo)
	// Server-to-server: relays join with a one-time token, then authenticate heartbeats
	// with their node credential.
	r.Post("/cluster/join", a.clusterJoin)
	r.Post("/cluster/heartbeat", a.clusterHeartbeat)
	r.Group(func(r chi.Router) {
		r.Use(a.requireUser)
		r.Get("/me", a.me)
		r.Put("/me/language", a.setLanguage)
		r.Put("/me/password", a.changePassword)
		r.Get("/dashboard", a.dashboard)
		r.Get("/devices", a.devices)
		r.Get("/customers", a.customers)
		r.Get("/system/info", a.systemInfo)
		if a.Gateway != nil {
			a.Gateway.PortalRoutes(r)
		}
		a.builderRoutes(r)
		r.Get("/sessions", a.listSessions)
		r.Get("/sessions/export.csv", a.exportSessions)
		r.Get("/sessions/{id}", a.getSession)
		r.Get("/sessions/{id}/recording", a.getRecording)
		r.Put("/sessions/{id}/recording/{seq}", a.putChunk)
		r.Post("/sessions/{id}/recording/finish", a.finishRecording)
		r.Post("/sessions/{id}/shares", a.createShare)
		r.Delete("/sessions/{id}/shares/{share}", a.revokeShare)
		r.Group(func(r chi.Router) {
			r.Use(requireAdmin)
			r.Get("/users", a.listUsers)
			r.Post("/users", a.createUser)
			r.Put("/users/{id}", a.updateUser)
			r.Put("/users/{id}/password", a.setUserPassword)
			r.Delete("/users/{id}", a.deleteUser)
			r.Get("/settings/mail", a.getMail)
			r.Put("/settings/mail", a.putMail)
			r.Post("/settings/mail/test", a.testMail)
			r.Get("/settings/backup", a.getBackup)
			r.Put("/settings/backup", a.putBackup)
			r.Post("/backup/test", a.testBackup)
			r.Post("/backup/run", a.runBackup)
			r.Get("/backup/runs", a.backupRuns)
			r.Get("/backup/remote", a.remoteBackups)
			r.Post("/backup/restore", a.restoreBackup)
			r.Get("/backup/restore", a.restoreStatus)
			r.Post("/system/check", a.checkUpdate)
			r.Put("/system/channel", a.setChannel)
			r.Post("/system/update", a.applyUpdate)
			r.Get("/audit", a.auditLog)
			r.Get("/settings/recording", a.getRecordingPolicy)
			r.Put("/settings/recording", a.putRecordingPolicy)
			r.Delete("/sessions/{id}/recording", a.deleteRecording)
			r.Get("/cluster/nodes", a.clusterNodes)
			r.Put("/cluster/nodes/{node}/drain", a.clusterDrain)
			r.Delete("/cluster/nodes/{node}", a.clusterRemove)
			r.Get("/cluster/tokens", a.clusterTokens)
			r.Post("/cluster/tokens", a.clusterCreateToken)
			r.Delete("/cluster/tokens/{id}", a.clusterRevokeToken)
			r.Post("/cluster/turn-secret/rotate", a.clusterRotateSecret)
		})
	})
	return r
}

// ---- helpers ----

type apiError struct {
	Error  string `json:"error"`
	Detail string `json:"detail,omitempty"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// fail sends a stable error code the portal can translate, plus detail for technical errors.
func fail(w http.ResponseWriter, code int, key string, err error) {
	e := apiError{Error: key}
	if err != nil {
		e.Detail = err.Error()
		if code >= 500 {
			slog.Error("request failed", "error", key, "err", err)
		}
	}
	writeJSON(w, code, e)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		fail(w, http.StatusBadRequest, "bad_request", err)
		return false
	}
	return true
}

func clientIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

func actor(r *http.Request) audit.Actor {
	a := audit.Actor{IP: clientIP(r)}
	if u := auth.FromContext(r.Context()); u != nil {
		a.UserID, a.Email = u.ID, u.Email
	}
	return a
}

// TrustedProxyIP is trustedProxyIP for routers outside this package (the agent API).
func TrustedProxyIP(trusted func(net.IP) bool) func(http.Handler) http.Handler {
	return trustedProxyIP(trusted)
}

// trustedProxyIP finds the real client address. Forwarding headers are only believed
// from trusted proxies: the local Caddy (loopback) and any trusted_proxies in the config,
// such as Zoraxy in front of it. X-Forwarded-For is walked from the right and the first
// address that is not a trusted proxy is the client; anything left of it could be spoofed
// by the client to dodge the login rate limit or fake the activity log.
func trustedProxyIP(trusted func(net.IP) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if err == nil && trusted(net.ParseIP(host)) {
				if ip := forwardedClient(r, trusted); ip != nil {
					r.RemoteAddr = net.JoinHostPort(ip.String(), "0")
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

func forwardedClient(r *http.Request, trusted func(net.IP) bool) net.IP {
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	if len(hops) == 0 {
		return net.ParseIP(strings.TrimSpace(r.Header.Get("X-Real-IP")))
	}
	var last net.IP
	for i := len(hops) - 1; i >= 0; i-- {
		ip := net.ParseIP(strings.TrimSpace(hops[i]))
		if ip == nil {
			break // garbage: stop at the last address a trusted proxy vouched for
		}
		last = ip
		if !trusted(ip) {
			return ip
		}
	}
	return last
}

// csrf requires a custom header on state-changing requests. Browsers cannot send it
// cross-site without a CORS preflight, which this API never allows.
func csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-Requested-With") != "rhis-remote" {
			fail(w, http.StatusForbidden, "csrf", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *API) requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(auth.CookieName)
		if err != nil {
			fail(w, http.StatusUnauthorized, "unauthorized", nil)
			return
		}
		u, err := a.Auth.UserForToken(r.Context(), c.Value)
		if err != nil {
			fail(w, http.StatusUnauthorized, "unauthorized", nil)
			return
		}
		next.ServeHTTP(w, r.WithContext(auth.WithUser(r.Context(), u)))
	})
}

func requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u := auth.FromContext(r.Context()); u == nil || !u.IsAdmin() {
			fail(w, http.StatusForbidden, "forbidden", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// loginLimiter allows 10 failed logins per IP per 15 minutes.
type loginLimiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

func (l *loginLimiter) blocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cut := time.Now().Add(-15 * time.Minute)
	keep := l.hits[ip][:0]
	for _, t := range l.hits[ip] {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	if l.hits == nil {
		l.hits = map[string][]time.Time{}
	}
	l.hits[ip] = keep
	return len(keep) >= 10
}

func (l *loginLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hits == nil {
		l.hits = map[string][]time.Time{}
	}
	l.hits[ip] = append(l.hits[ip], time.Now())
}

// ---- public ----

func (a *API) meta(w http.ResponseWriter, r *http.Request) {
	n, err := a.Auth.UserCount(r.Context())
	if err != nil {
		fail(w, 500, "internal", err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"product_name":   a.Cfg.Branding.ProductName,
		"company_name":   a.Cfg.Branding.CompanyName,
		"setup_required": n == 0,
		"version":        version.Version,
	})
}

func (a *API) setup(w http.ResponseWriter, r *http.Request) {
	var in struct{ Email, Name, Password, Language string }
	if !decode(w, r, &in) {
		return
	}
	if !strings.Contains(in.Email, "@") || strings.TrimSpace(in.Name) == "" {
		fail(w, 400, "invalid_input", nil)
		return
	}
	u, err := a.Auth.CreateFirstAdmin(r.Context(), in.Email, in.Name, in.Password, in.Language)
	if err != nil {
		fail(w, 400, err.Error(), nil)
		return
	}
	a.Audit.Record(r.Context(), audit.Actor{UserID: u.ID, Email: u.Email, IP: clientIP(r)}, "setup.admin_created", u.Email, nil)
	_, tok, err := a.Auth.Login(r.Context(), in.Email, in.Password, clientIP(r), r.UserAgent())
	if err != nil {
		fail(w, 500, "internal", err)
		return
	}
	a.Auth.SetCookie(w, tok)
	writeJSON(w, 200, u)
}

func (a *API) login(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if a.limiter.blocked(ip) {
		fail(w, http.StatusTooManyRequests, "too_many_attempts", nil)
		return
	}
	var in struct{ Email, Password string }
	if !decode(w, r, &in) {
		return
	}
	u, tok, err := a.Auth.Login(r.Context(), in.Email, in.Password, ip, r.UserAgent())
	if errors.Is(err, auth.ErrInvalidLogin) {
		a.limiter.fail(ip)
		a.Audit.Record(r.Context(), audit.Actor{IP: ip}, "auth.login_failed", strings.ToLower(in.Email), nil)
		fail(w, 401, "invalid_login", nil)
		return
	}
	if err != nil {
		fail(w, 500, "internal", err)
		return
	}
	a.Auth.SetCookie(w, tok)
	a.Audit.Record(r.Context(), audit.Actor{UserID: u.ID, Email: u.Email, IP: ip}, "auth.login", "", nil)
	writeJSON(w, 200, u)
}

func (a *API) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(auth.CookieName); err == nil {
		a.Auth.Logout(r.Context(), c.Value)
	}
	a.Auth.ClearCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// ---- current user ----

func (a *API) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, auth.FromContext(r.Context()))
}

func (a *API) setLanguage(w http.ResponseWriter, r *http.Request) {
	var in struct{ Language string }
	if !decode(w, r, &in) {
		return
	}
	if err := a.Auth.SetLanguage(r.Context(), auth.FromContext(r.Context()).ID, in.Language); err != nil {
		fail(w, 500, "internal", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) changePassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if !decode(w, r, &in) {
		return
	}
	u := auth.FromContext(r.Context())
	if err := a.Auth.Verify(r.Context(), u.ID, in.Current); err != nil {
		fail(w, 400, "invalid_login", nil)
		return
	}
	if err := a.Auth.SetPassword(r.Context(), u.ID, in.New); err != nil {
		fail(w, 400, err.Error(), nil)
		return
	}
	a.Audit.Record(r.Context(), actor(r), "auth.password_changed", u.Email, nil)
	a.Auth.ClearCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// ---- overview ----

func (a *API) dashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var out struct {
		Clients        int         `json:"clients"`
		Online         int         `json:"online"`
		Offline        int         `json:"offline"`
		OfflineWeek    int         `json:"offline_over_week"`
		ActiveSessions int         `json:"active_sessions"`
		Customers      int         `json:"customers"`
		LastBackup     *backup.Run `json:"last_backup"`
	}
	err := a.DB.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE online), count(*) FILTER (WHERE NOT online),
		count(*) FILTER (WHERE NOT online AND (last_seen_at IS NULL OR last_seen_at < now() - interval '7 days')) FROM devices WHERE kind='agent'`).
		Scan(&out.Clients, &out.Online, &out.Offline, &out.OfflineWeek)
	if err == nil {
		err = a.DB.QueryRow(ctx, `SELECT count(*) FROM remote_sessions WHERE ended_at IS NULL`).Scan(&out.ActiveSessions)
	}
	if err == nil {
		err = a.DB.QueryRow(ctx, `SELECT count(*) FROM customers`).Scan(&out.Customers)
	}
	if err != nil {
		fail(w, 500, "internal", err)
		return
	}
	if runs, err := a.Backup.Runs(ctx, 1); err == nil && len(runs) > 0 {
		out.LastBackup = &runs[0]
	}
	writeJSON(w, 200, out)
}

func (a *API) devices(w http.ResponseWriter, r *http.Request) {
	rows, err := a.DB.Query(r.Context(), `SELECT d.id, d.device_code, d.hostname, d.os_version, d.agent_version, d.last_user, d.last_seen_at, d.online, c.name
		FROM devices d LEFT JOIN customers c ON c.id=d.customer_id WHERE d.kind='agent' ORDER BY d.online DESC, d.hostname`)
	if err != nil {
		fail(w, 500, "internal", err)
		return
	}
	defer rows.Close()
	type dev struct {
		ID           int64      `json:"id"`
		Code         string     `json:"code"`
		Hostname     string     `json:"hostname"`
		OSVersion    *string    `json:"os_version"`
		AgentVersion *string    `json:"agent_version"`
		LastUser     *string    `json:"last_user"`
		LastSeen     *time.Time `json:"last_seen_at"`
		Online       bool       `json:"online"`
		Customer     *string    `json:"customer"`
	}
	out := []dev{}
	for rows.Next() {
		var d dev
		if err := rows.Scan(&d.ID, &d.Code, &d.Hostname, &d.OSVersion, &d.AgentVersion, &d.LastUser, &d.LastSeen, &d.Online, &d.Customer); err != nil {
			fail(w, 500, "internal", err)
			return
		}
		out = append(out, d)
	}
	writeJSON(w, 200, out)
}

func (a *API) customers(w http.ResponseWriter, r *http.Request) {
	rows, err := a.DB.Query(r.Context(), `SELECT c.id, c.name, count(d.id), count(d.id) FILTER (WHERE d.online)
		FROM customers c LEFT JOIN devices d ON d.customer_id=c.id AND d.kind='agent' GROUP BY c.id ORDER BY c.name`)
	if err != nil {
		fail(w, 500, "internal", err)
		return
	}
	defer rows.Close()
	type cust struct {
		ID      int64  `json:"id"`
		Name    string `json:"name"`
		Devices int    `json:"devices"`
		Online  int    `json:"online"`
	}
	out := []cust{}
	for rows.Next() {
		var c cust
		if err := rows.Scan(&c.ID, &c.Name, &c.Devices, &c.Online); err != nil {
			fail(w, 500, "internal", err)
			return
		}
		out = append(out, c)
	}
	writeJSON(w, 200, out)
}

func (a *API) auditLog(w http.ResponseWriter, r *http.Request) {
	es, err := a.Audit.List(r.Context(), 200, 0)
	if err != nil {
		fail(w, 500, "internal", err)
		return
	}
	writeJSON(w, 200, es)
}

// background runs fn detached from the request, with a generous timeout.
func background(fn func(ctx context.Context)) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
		defer cancel()
		fn(ctx)
	}()
}
