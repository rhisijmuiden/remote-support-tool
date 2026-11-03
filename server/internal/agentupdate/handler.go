// SPDX-License-Identifier: AGPL-3.0-only

package agentupdate

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"golang.org/x/mod/semver"
)

// DeviceAuth identifies the device behind an agent request (Bearer device token).
type DeviceAuth interface {
	AuthenticateDevice(r *http.Request) (deviceID int64, customerID *int64, err error)
}

// SQLDeviceAuth looks the token up in devices.token_hash (SHA-256 of the token).
// It is a stand-in until the agents package provides DeviceAuth.
type SQLDeviceAuth struct{ S *Service }

func (a SQLDeviceAuth) AuthenticateDevice(r *http.Request) (int64, *int64, error) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || len(tok) < 20 {
		return 0, nil, errors.New("unauthorized")
	}
	h := sha256.Sum256([]byte(tok))
	var id int64
	var cust *int64
	err := a.S.DB.QueryRow(r.Context(), `SELECT id, customer_id FROM devices WHERE token_hash=$1`, h[:]).Scan(&id, &cust)
	if err != nil {
		return 0, nil, errors.New("unauthorized")
	}
	return id, cust, nil
}

// Handler serves the agent-facing update endpoints, mounted at /api/agent/v1.
type Handler struct {
	S         *Service
	Auth      DeviceAuth
	PublicURL string
	// ManifestTTL is how long a signed manifest stays valid.
	ManifestTTL time.Duration
}

func (h *Handler) Mount(r chi.Router) {
	r.Get("/update", h.check)
	r.Post("/update/report", h.report)
	r.Get("/files/{sha256}", h.file)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (h *Handler) device(w http.ResponseWriter, r *http.Request) (int64, *int64, bool) {
	id, cust, err := h.Auth.AuthenticateDevice(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return 0, nil, false
	}
	return id, cust, true
}

// check answers GET /update?product=agent&os=windows&arch=amd64&version=v0.2.0&channel=production.
func (h *Handler) check(w http.ResponseWriter, r *http.Request) {
	id, cust, ok := h.device(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	product, osName, arch, cur, ch := q.Get("product"), q.Get("os"), q.Get("arch"), q.Get("version"), q.Get("channel")
	if product == "" {
		product = "agent"
	}
	if ch != "test" {
		ch = "production"
	}
	if (product != "agent" && product != "quick") || osName != "windows" || (arch != "amd64" && arch != "arm64") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request"})
		return
	}
	m, err := h.S.Offer(r.Context(), id, cust, product, arch, cur, ch, h.PublicURL, h.ManifestTTL)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
		return
	}
	if m == nil {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	sg, err := h.S.Signer.Sign(m)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
		return
	}
	writeJSON(w, http.StatusOK, sg)
}

// Offer works out the update for one device and returns the manifest to sign, or nil.
func (s *Service) Offer(ctx context.Context, deviceID int64, customerID *int64, product, arch, current, channel, publicURL string, ttl time.Duration) (*Manifest, error) {
	p, err := s.Policy(ctx)
	if err != nil {
		return nil, err
	}
	o, err := s.override(ctx, customerID)
	if err != nil {
		return nil, err
	}
	rs, err := s.releasesFor(ctx, product, "windows", arch)
	if err != nil {
		return nil, err
	}
	d := Decide(p, o, deviceID, current, channel, rs)
	if d.Release == nil {
		return nil, nil
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	now := time.Now().UTC().Truncate(time.Second)
	name := "rhis-agent"
	if product == "quick" {
		name = "rhis-quick"
	}
	m := &Manifest{Product: name, Version: d.Release.Version, OS: "windows", Arch: arch, Size: d.Release.Size,
		SHA256: d.Release.SHA256, URL: strings.TrimRight(publicURL, "/") + "/api/agent/v1/files/" + d.Release.SHA256,
		ReleasedAt: d.Release.ReleasedAt.UTC(), IssuedAt: now, ExpiresAt: now.Add(ttl), DeviceID: deviceID, AllowDowngrade: d.AllowDowngrade}
	// Log an offer once a day per device and version, so polling does not flood the log.
	s.DB.Exec(ctx, `INSERT INTO agent_update_events(device_id, from_version, to_version, status)
		SELECT $1, NULLIF($2,''), $3, 'offered' WHERE NOT EXISTS (SELECT 1 FROM agent_update_events
		WHERE device_id=$1 AND to_version=$3 AND status='offered' AND at > now() - interval '1 day')`, deviceID, CanonVersion(current), m.Version)
	return m, nil
}

func (h *Handler) report(w http.ResponseWriter, r *http.Request) {
	id, _, ok := h.device(w, r)
	if !ok {
		return
	}
	var in struct {
		From   string `json:"from_version"`
		To     string `json:"to_version"`
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || CanonVersion(in.To) == "" ||
		(in.Status != "downloaded" && in.Status != "installed" && in.Status != "failed") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request"})
		return
	}
	if len(in.Error) > 1000 {
		in.Error = in.Error[:1000]
	}
	if _, err := h.S.DB.Exec(r.Context(), `INSERT INTO agent_update_events(device_id, from_version, to_version, status, error)
		VALUES($1, NULLIF($2,''), $3, $4, NULLIF($5,''))`, id, CanonVersion(in.From), CanonVersion(in.To), in.Status, in.Error); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) file(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.device(w, r); !ok {
		return
	}
	sum := chi.URLParam(r, "sha256")
	if !sumRe.MatchString(sum) {
		http.NotFound(w, r)
		return
	}
	var known bool
	if err := h.S.DB.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM agent_releases WHERE sha256=$1)`, sum).Scan(&known); err != nil || !known {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(h.S.Path(sum))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.microsoft.portable-executable")
	w.Header().Set("Cache-Control", "private, max-age=86400, immutable")
	http.ServeContent(w, r, sum+".exe", st.ModTime(), f)
}

// Overview is the rollout status for the portal.
type Overview struct {
	PublicKey   string         `json:"public_key"`
	Fingerprint string         `json:"fingerprint"`
	Versions    []VersionCount `json:"versions"`
	Events      []Event        `json:"events"`
}

type VersionCount struct {
	Version string `json:"version"`
	Devices int    `json:"devices"`
	Online  int    `json:"online"`
}

type Event struct {
	At       time.Time `json:"at"`
	DeviceID *int64    `json:"device_id"`
	Hostname *string   `json:"hostname"`
	From     *string   `json:"from_version"`
	To       string    `json:"to_version"`
	Status   string    `json:"status"`
	Error    *string   `json:"error"`
}

func (s *Service) Overview(ctx context.Context) (*Overview, error) {
	out := &Overview{PublicKey: s.Signer.PublicKey(), Fingerprint: s.Signer.Fingerprint()}
	rows, err := s.DB.Query(ctx, `SELECT coalesce(agent_version,''), count(*), count(*) FILTER (WHERE online) FROM devices GROUP BY 1`)
	if err != nil {
		return nil, err
	}
	out.Versions, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (VersionCount, error) {
		var v VersionCount
		return v, r.Scan(&v.Version, &v.Devices, &v.Online)
	})
	if err != nil {
		return nil, err
	}
	sortVersions(out.Versions)
	rows, err = s.DB.Query(ctx, `SELECT e.at, e.device_id, d.hostname, e.from_version, e.to_version, e.status, e.error
		FROM agent_update_events e LEFT JOIN devices d ON d.id=e.device_id ORDER BY e.at DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	out.Events, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Event, error) {
		var e Event
		return e, r.Scan(&e.At, &e.DeviceID, &e.Hostname, &e.From, &e.To, &e.Status, &e.Error)
	})
	return out, err
}

func sortVersions(v []VersionCount) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && newer(v[j].Version, v[j-1].Version); j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

func newer(a, b string) bool {
	ca, cb := CanonVersion(a), CanonVersion(b)
	if ca == "" || cb == "" {
		return ca != "" && cb == ""
	}
	return semver.Compare(ca, cb) > 0
}
