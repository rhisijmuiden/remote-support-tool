package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/rhisijmuiden/remote-support-tool/server/internal/audit"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/auth"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/cluster"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/version"
)

func (a *API) clusterNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := a.Cluster.Nodes(r.Context())
	if err != nil {
		fail(w, 500, "internal", err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"nodes":         nodes,
		"version":       version.Version,
		"bus":           a.busKind(),
		"offline_after": int(cluster.OfflineAfter.Seconds()),
	})
}

func (a *API) busKind() string {
	if a.Cfg.Cluster.NATS.URL != "" {
		return "nats"
	}
	return "local"
}

func (a *API) clusterDrain(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Draining bool `json:"draining"`
	}
	if !decode(w, r, &in) {
		return
	}
	id := chi.URLParam(r, "node")
	if err := a.Cluster.SetDraining(r.Context(), id, in.Draining); err != nil {
		clusterFail(w, err)
		return
	}
	action := "cluster.node_resumed"
	if in.Draining {
		action = "cluster.node_drained"
	}
	a.Audit.Record(r.Context(), actor(r), action, id, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) clusterRemove(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "node")
	if err := a.Cluster.Remove(r.Context(), id); err != nil {
		clusterFail(w, err)
		return
	}
	a.Audit.Record(r.Context(), actor(r), "cluster.node_removed", id, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) clusterTokens(w http.ResponseWriter, r *http.Request) {
	ts, err := a.Cluster.JoinTokens(r.Context())
	if err != nil {
		fail(w, 500, "internal", err)
		return
	}
	writeJSON(w, 200, ts)
}

func (a *API) clusterCreateToken(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name     string   `json:"name"`
		Roles    []string `json:"roles"`
		TTLHours int      `json:"ttl_hours"`
		Domain   string   `json:"domain"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Domain != "" && !validHost(in.Domain) {
		fail(w, 400, "invalid_domain", nil)
		return
	}
	u := auth.FromContext(r.Context())
	tok, jt, err := a.Cluster.CreateJoinToken(r.Context(), in.Name, in.Roles, time.Duration(in.TTLHours)*time.Hour, u.ID)
	if err != nil {
		clusterFail(w, err)
		return
	}
	st, _ := a.Update.Settings(r.Context())
	a.Audit.Record(r.Context(), actor(r), "cluster.token_created", jt.Name, map[string]any{"roles": jt.Roles, "expires_at": jt.ExpiresAt})
	writeJSON(w, 200, map[string]any{
		"token":   tok,
		"join":    jt,
		"command": a.joinCommand(st.Channel, tok, in.Domain),
	})
}

// joinCommand is the one-liner shown in the portal to install a new relay.
func (a *API) joinCommand(channel, token, domain string) string {
	branch := "main"
	if channel == "test" {
		branch = "test"
	}
	cmd := fmt.Sprintf("curl -fsSL https://raw.githubusercontent.com/%s/%s/deploy/debian/install.sh | sudo bash -s -- --role relay --channel %s --control %s --token %s",
		a.Cfg.Update.Repo, branch, channel, strings.TrimRight(a.Cfg.PublicURL, "/"), token)
	if domain != "" {
		cmd += " --relay-domain " + domain
	}
	return cmd
}

func validHost(h string) bool {
	if len(h) > 253 || !strings.Contains(h, ".") {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

func (a *API) clusterRevokeToken(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		fail(w, 400, "bad_request", err)
		return
	}
	if err := a.Cluster.RevokeJoinToken(r.Context(), id); err != nil {
		clusterFail(w, err)
		return
	}
	a.Audit.Record(r.Context(), actor(r), "cluster.token_revoked", strconv.FormatInt(id, 10), nil)
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) clusterRotateSecret(w http.ResponseWriter, r *http.Request) {
	if err := a.Cluster.RotateTURNSecret(r.Context()); err != nil {
		fail(w, 500, "internal", err)
		return
	}
	a.Audit.Record(r.Context(), actor(r), "cluster.turn_secret_rotated", "", nil)
	w.WriteHeader(http.StatusNoContent)
}

// ---- server-to-server ----

func (a *API) clusterJoin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if a.limiter.blocked(ip) {
		fail(w, http.StatusTooManyRequests, "too_many_attempts", nil)
		return
	}
	var in cluster.JoinRequest
	if !decode(w, r, &in) {
		return
	}
	resp, err := a.Cluster.Join(r.Context(), in, ip)
	if errors.Is(err, cluster.ErrInvalidToken) {
		a.limiter.fail(ip)
		a.Audit.Record(r.Context(), audit.Actor{IP: ip}, "cluster.join_failed", in.Name, nil)
		fail(w, 400, "invalid_token", nil)
		return
	}
	if err != nil {
		fail(w, 500, "internal", err)
		return
	}
	a.Audit.Record(r.Context(), audit.Actor{IP: ip}, "cluster.node_joined", resp.NodeID, map[string]any{"name": in.Name, "roles": resp.Roles})
	writeJSON(w, 200, resp)
}

func (a *API) clusterHeartbeat(w http.ResponseWriter, r *http.Request) {
	bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	ip := clientIP(r)
	if !ok {
		fail(w, 401, "unauthorized", nil)
		return
	}
	if a.limiter.blocked(ip) {
		fail(w, http.StatusTooManyRequests, "too_many_attempts", nil)
		return
	}
	id, err := a.Cluster.Authenticate(r.Context(), bearer)
	if err != nil {
		a.limiter.fail(ip)
		fail(w, 401, "unauthorized", nil)
		return
	}
	var in cluster.HeartbeatRequest
	if !decode(w, r, &in) {
		return
	}
	resp, err := a.Cluster.Heartbeat(r.Context(), id, in, ip)
	if err != nil {
		clusterFail(w, err)
		return
	}
	writeJSON(w, 200, resp)
}

func clusterFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, cluster.ErrNotFound):
		fail(w, 404, "not_found", nil)
	case errors.Is(err, cluster.ErrUnauthorized):
		fail(w, 401, "unauthorized", nil)
	case errors.Is(err, cluster.ErrBadRole), err.Error() == "invalid_name", err.Error() == "cannot_remove_self":
		fail(w, 400, err.Error(), nil)
	default:
		fail(w, 500, "internal", err)
	}
}
