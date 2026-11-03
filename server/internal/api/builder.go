// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/rhisijmuiden/remote-support-tool/server/internal/agentupdate"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/auth"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/builder"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/codesign"
)

// builderRoutes registers the client builder and agent update endpoints. Technicians can
// make installers and download links; only administrators change builds, releases and policy.
func (a *API) builderRoutes(r chi.Router) {
	if a.Builder == nil || a.Agents == nil {
		return
	}
	r.Get("/builds", a.listBuilds)
	r.Get("/builds/{id}", a.getBuild)
	r.Get("/builds/{id}/logo", a.getLogo)
	r.Post("/builds/{id}/files", a.generateFile)
	r.Get("/builds/{id}/files/{fid}/download", a.downloadFile)
	r.Post("/builds/{id}/files/{fid}/links", a.createLink)
	r.Delete("/builds/{id}/links/{lid}", a.revokeLink)
	r.Get("/agent/releases", a.listReleases)
	r.Group(func(r chi.Router) {
		r.Use(requireAdmin)
		r.Post("/customers", a.createCustomer)
		r.Put("/customers/{id}", a.renameCustomer)
		r.Delete("/customers/{id}", a.deleteCustomer)
		r.Post("/builds", a.createBuild)
		r.Put("/builds/{id}", a.updateBuild)
		r.Delete("/builds/{id}", a.deleteBuild)
		r.Put("/builds/{id}/logo", a.putLogo)
		r.Delete("/builds/{id}/logo", a.deleteLogo)
		r.Post("/builds/{id}/revoke", a.revokeBuild)
		r.Post("/builds/{id}/restore", a.unrevokeBuild)
		r.Post("/builds/{id}/rotate-token", a.rotateToken)
		r.Delete("/builds/{id}/files/{fid}", a.deleteFile)
		r.Post("/agent/releases/sync", a.syncReleases)
		r.Post("/agent/releases/upload", a.uploadRelease)
		r.Delete("/agent/releases/{id}", a.deleteRelease)
		r.Get("/agent/policy", a.getPolicy)
		r.Put("/agent/policy", a.putPolicy)
		r.Put("/agent/policy/customers/{id}", a.putOverride)
		r.Get("/agent/overview", a.agentOverview)
		r.Get("/settings/codesign", a.getCodesign)
		r.Put("/settings/codesign", a.putCodesign)
	})
}

// failCode maps service errors that are stable codes to 400, everything else to 500.
func failCode(w http.ResponseWriter, err error) {
	msg := err.Error()
	code, _, _ := strings.Cut(msg, ":")
	switch code {
	case "not_found":
		fail(w, 404, code, nil)
	case "build_name_required", "product_name_required", "invalid_mode", "invalid_channel", "invalid_color",
		"servers_required", "too_many_servers", "server_invalid", "server_not_https", "invalid_customer", "logo_too_large",
		"logo_not_png", "logo_size", "invalid_arch", "build_revoked", "no_agent_release", "invalid_link_days", "invalid_link_max",
		"invalid_product", "invalid_version", "invalid_file", "not_windows_exe", "file_is_client_build", "release_exists",
		"release_pinned", "invalid_rollout", "sync_busy", "customer_name_required",
		"codesign_timestamp", "codesign_pfx_invalid", "codesign_pfx_required", "codesign_pfx_password",
		"codesign_not_code_signing", "codesign_expired", "codesign_command", "codesign_mode":
		fail(w, 400, code, nil)
	case "codesign_failed":
		fail(w, 502, code, errors.Unwrap(err))
	default:
		fail(w, 500, "internal", err)
	}
}

func param(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, name), 10, 64)
	if err != nil {
		fail(w, 400, "bad_request", err)
		return 0, false
	}
	return id, true
}

func userID(r *http.Request) int64 {
	if u := auth.FromContext(r.Context()); u != nil {
		return u.ID
	}
	return 0
}

// ---- customers ----

func (a *API) createCustomer(w http.ResponseWriter, r *http.Request) {
	var in struct{ Name string }
	if !decode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 120 {
		fail(w, 400, "customer_name_required", nil)
		return
	}
	var id int64
	if err := a.DB.QueryRow(r.Context(), `INSERT INTO customers(name) VALUES($1) RETURNING id`, in.Name).Scan(&id); err != nil {
		fail(w, 500, "internal", err)
		return
	}
	a.Audit.Record(r.Context(), actor(r), "customer.created", in.Name, nil)
	writeJSON(w, 201, map[string]any{"id": id, "name": in.Name})
}

func (a *API) renameCustomer(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	var in struct{ Name string }
	if !decode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 120 {
		fail(w, 400, "customer_name_required", nil)
		return
	}
	if _, err := a.DB.Exec(r.Context(), `UPDATE customers SET name=$2 WHERE id=$1`, id, in.Name); err != nil {
		fail(w, 500, "internal", err)
		return
	}
	a.Audit.Record(r.Context(), actor(r), "customer.renamed", in.Name, nil)
	w.WriteHeader(204)
}

func (a *API) deleteCustomer(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	var name string
	if err := a.DB.QueryRow(r.Context(), `DELETE FROM customers WHERE id=$1 RETURNING name`, id).Scan(&name); err != nil {
		fail(w, 404, "not_found", nil)
		return
	}
	a.Audit.Record(r.Context(), actor(r), "customer.deleted", name, nil)
	w.WriteHeader(204)
}

// ---- builds ----

func (a *API) listBuilds(w http.ResponseWriter, r *http.Request) {
	bs, err := a.Builder.List(r.Context())
	if err != nil {
		failCode(w, err)
		return
	}
	writeJSON(w, 200, bs)
}

func (a *API) getBuild(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	b, err := a.Builder.Get(r.Context(), id)
	if err != nil {
		failCode(w, err)
		return
	}
	writeJSON(w, 200, b)
}

func (a *API) createBuild(w http.ResponseWriter, r *http.Request) {
	var in builder.Input
	if !decode(w, r, &in) {
		return
	}
	b, err := a.Builder.Create(r.Context(), in, userID(r))
	if err != nil {
		failCode(w, err)
		return
	}
	a.Audit.Record(r.Context(), actor(r), "build.created", b.Name, map[string]any{"mode": b.Mode, "servers": b.Servers})
	writeJSON(w, 201, b)
}

func (a *API) updateBuild(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	var in builder.Input
	if !decode(w, r, &in) {
		return
	}
	if err := a.Builder.Update(r.Context(), id, in); err != nil {
		failCode(w, err)
		return
	}
	a.Audit.Record(r.Context(), actor(r), "build.updated", in.Name, map[string]any{"servers": in.Servers, "channel": in.Channel})
	w.WriteHeader(204)
}

func (a *API) deleteBuild(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	if err := a.Builder.Delete(r.Context(), id); err != nil {
		failCode(w, err)
		return
	}
	a.Audit.Record(r.Context(), actor(r), "build.deleted", strconv.FormatInt(id, 10), nil)
	w.WriteHeader(204)
}

func (a *API) getLogo(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	b, err := a.Builder.Logo(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Write(b)
}

func (a *API) putLogo(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, builder.MaxLogoUpload))
	if err != nil {
		fail(w, 400, "logo_too_large", nil)
		return
	}
	if err := a.Builder.SetLogo(r.Context(), id, raw); err != nil {
		failCode(w, err)
		return
	}
	a.Audit.Record(r.Context(), actor(r), "build.logo_changed", strconv.FormatInt(id, 10), nil)
	w.WriteHeader(204)
}

func (a *API) deleteLogo(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	if err := a.Builder.SetLogo(r.Context(), id, nil); err != nil {
		failCode(w, err)
		return
	}
	w.WriteHeader(204)
}

func (a *API) revokeBuild(w http.ResponseWriter, r *http.Request)   { a.setRevoked(w, r, true) }
func (a *API) unrevokeBuild(w http.ResponseWriter, r *http.Request) { a.setRevoked(w, r, false) }

func (a *API) setRevoked(w http.ResponseWriter, r *http.Request, v bool) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	if err := a.Builder.SetRevoked(r.Context(), id, v); err != nil {
		failCode(w, err)
		return
	}
	act := "build.revoked"
	if !v {
		act = "build.restored"
	}
	a.Audit.Record(r.Context(), actor(r), act, strconv.FormatInt(id, 10), nil)
	w.WriteHeader(204)
}

func (a *API) rotateToken(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	if err := a.Builder.RotateToken(r.Context(), id); err != nil {
		failCode(w, err)
		return
	}
	a.Audit.Record(r.Context(), actor(r), "build.token_rotated", strconv.FormatInt(id, 10), nil)
	w.WriteHeader(204)
}

func (a *API) generateFile(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	var in struct{ Arch string }
	if !decode(w, r, &in) {
		return
	}
	if in.Arch == "" {
		in.Arch = "amd64"
	}
	f, err := a.Builder.Generate(r.Context(), id, in.Arch, userID(r))
	if err != nil {
		failCode(w, err)
		return
	}
	a.Audit.Record(r.Context(), actor(r), "build.file_generated", f.FileName, map[string]any{"build": id, "agent": f.AgentVersion, "signed": f.Signed})
	writeJSON(w, 201, f)
}

func (a *API) downloadFile(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	fid, ok := param(w, r, "fid")
	if !ok {
		return
	}
	a.Audit.Record(r.Context(), actor(r), "build.file_downloaded", strconv.FormatInt(fid, 10), map[string]any{"build": id})
	a.Builder.ServeFile(w, r, id, fid)
}

func (a *API) deleteFile(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	fid, ok := param(w, r, "fid")
	if !ok {
		return
	}
	if err := a.Builder.DeleteFile(r.Context(), id, fid); err != nil {
		failCode(w, err)
		return
	}
	w.WriteHeader(204)
}

func (a *API) createLink(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	fid, ok := param(w, r, "fid")
	if !ok {
		return
	}
	var in struct {
		Days         int  `json:"days"`
		MaxDownloads *int `json:"max_downloads"`
	}
	if !decode(w, r, &in) {
		return
	}
	tok, err := a.Builder.CreateLink(r.Context(), id, fid, in.Days, in.MaxDownloads, userID(r))
	if err != nil {
		failCode(w, err)
		return
	}
	a.Audit.Record(r.Context(), actor(r), "build.link_created", strconv.FormatInt(fid, 10), map[string]any{"build": id, "days": in.Days})
	writeJSON(w, 201, map[string]string{"url": strings.TrimRight(a.Cfg.PublicURL, "/") + "/d/" + tok})
}

func (a *API) revokeLink(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	lid, ok := param(w, r, "lid")
	if !ok {
		return
	}
	if err := a.Builder.RevokeLink(r.Context(), id, lid); err != nil {
		failCode(w, err)
		return
	}
	a.Audit.Record(r.Context(), actor(r), "build.link_revoked", strconv.FormatInt(lid, 10), nil)
	w.WriteHeader(204)
}

// ---- agent releases and update policy ----

func (a *API) listReleases(w http.ResponseWriter, r *http.Request) {
	rs, err := a.Agents.Releases(r.Context())
	if err != nil {
		failCode(w, err)
		return
	}
	writeJSON(w, 200, rs)
}

func (a *API) syncReleases(w http.ResponseWriter, r *http.Request) {
	res, err := a.Agents.Sync(r.Context())
	if err != nil {
		if err.Error() == "sync_busy" {
			failCode(w, err)
			return
		}
		fail(w, 502, "agent_sync_failed", err)
		return
	}
	for _, rel := range res.Imported {
		a.Audit.Record(r.Context(), actor(r), "agent.release_imported", rel.Version, map[string]string{"product": rel.Product, "arch": rel.Arch})
	}
	writeJSON(w, 200, res)
}

func (a *API) uploadRelease(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	exe, err := io.ReadAll(http.MaxBytesReader(w, r.Body, agentupdate.MaxFileBytes))
	if err != nil {
		fail(w, 400, "invalid_file", nil)
		return
	}
	rel, err := a.Agents.Import(r.Context(), agentupdate.ImportInput{Product: q.Get("product"), Version: q.Get("version"),
		Arch: q.Get("arch"), Channel: q.Get("channel"), Source: "upload", Notes: q.Get("notes")}, exe)
	if err != nil {
		failCode(w, err)
		return
	}
	a.Audit.Record(r.Context(), actor(r), "agent.release_uploaded", rel.Version, map[string]string{"product": rel.Product, "arch": rel.Arch, "sha256": rel.SHA256})
	writeJSON(w, 201, rel)
}

func (a *API) deleteRelease(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	if err := a.Agents.Delete(r.Context(), id); err != nil {
		failCode(w, err)
		return
	}
	a.Audit.Record(r.Context(), actor(r), "agent.release_deleted", strconv.FormatInt(id, 10), nil)
	w.WriteHeader(204)
}

func (a *API) getPolicy(w http.ResponseWriter, r *http.Request) {
	p, err := a.Agents.Policy(r.Context())
	if err != nil {
		failCode(w, err)
		return
	}
	os, err := a.Agents.Overrides(r.Context())
	if err != nil {
		failCode(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"policy": p, "customers": os})
}

func (a *API) putPolicy(w http.ResponseWriter, r *http.Request) {
	var p agentupdate.Policy
	if !decode(w, r, &p) {
		return
	}
	if err := a.Agents.SetPolicy(r.Context(), p); err != nil {
		failCode(w, err)
		return
	}
	a.Audit.Record(r.Context(), actor(r), "agent.policy_changed", "", p)
	w.WriteHeader(204)
}

func (a *API) putOverride(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	var in struct{ Mode, Version string }
	if !decode(w, r, &in) {
		return
	}
	if err := a.Agents.SetOverride(r.Context(), id, in.Mode, in.Version); err != nil {
		if strings.Contains(err.Error(), "foreign key") {
			fail(w, 404, "not_found", nil)
			return
		}
		failCode(w, err)
		return
	}
	a.Audit.Record(r.Context(), actor(r), "agent.customer_policy_changed", strconv.FormatInt(id, 10), in)
	w.WriteHeader(204)
}

func (a *API) agentOverview(w http.ResponseWriter, r *http.Request) {
	o, err := a.Agents.Overview(r.Context())
	if err != nil {
		failCode(w, err)
		return
	}
	writeJSON(w, 200, o)
}

// ---- code signing ----

func (a *API) getCodesign(w http.ResponseWriter, r *http.Request) {
	st, err := a.CodeSign.Status(r.Context())
	if err != nil {
		failCode(w, err)
		return
	}
	writeJSON(w, 200, st)
}

func (a *API) putCodesign(w http.ResponseWriter, r *http.Request) {
	var in codesign.Update
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		fail(w, 400, "codesign_pfx_invalid", nil)
		return
	}
	if err := a.CodeSign.Save(r.Context(), in); err != nil {
		failCode(w, err)
		return
	}
	st, _ := a.CodeSign.Status(r.Context())
	a.Audit.Record(r.Context(), actor(r), "settings.codesign_saved", st.Subject, map[string]any{"mode": st.Mode, "thumbprint": st.Thumbprint})
	writeJSON(w, 200, st)
}

// ---- public download links ----

// PublicDownload serves GET /d/{token}, outside the API and without a session.
func (a *API) PublicDownload(w http.ResponseWriter, r *http.Request) {
	tok := r.PathValue("token")
	a.Builder.PublicDownload(w, r, tok)
}
