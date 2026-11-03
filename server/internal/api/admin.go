package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/rhisijmuiden/remote-support-tool/server/internal/audit"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/auth"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/backup"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/cluster"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/mail"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/version"
)

func idParam(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		fail(w, 400, "bad_request", err)
		return 0, false
	}
	return id, true
}

// ---- users ----

func (a *API) listUsers(w http.ResponseWriter, r *http.Request) {
	us, err := a.Auth.ListUsers(r.Context())
	if err != nil {
		fail(w, 500, "internal", err)
		return
	}
	writeJSON(w, 200, us)
}

func (a *API) createUser(w http.ResponseWriter, r *http.Request) {
	var in struct{ Email, Name, Password, Role, Language string }
	if !decode(w, r, &in) {
		return
	}
	if !strings.Contains(in.Email, "@") || strings.TrimSpace(in.Name) == "" {
		fail(w, 400, "invalid_input", nil)
		return
	}
	u, err := a.Auth.CreateUser(r.Context(), in.Email, in.Name, in.Password, in.Role, in.Language)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			fail(w, 409, "email_taken", nil)
			return
		}
		fail(w, 400, err.Error(), nil)
		return
	}
	a.Audit.Record(r.Context(), actor(r), "user.created", u.Email, map[string]string{"role": u.Role})
	writeJSON(w, 201, u)
}

func (a *API) updateUser(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	var in struct{ Name, Role, Language string }
	if !decode(w, r, &in) {
		return
	}
	if id == auth.FromContext(r.Context()).ID && in.Role != "admin" {
		fail(w, 400, "cannot_demote_self", nil)
		return
	}
	if err := a.Auth.UpdateUser(r.Context(), id, in.Name, in.Role, in.Language); err != nil {
		fail(w, 400, err.Error(), nil)
		return
	}
	a.Audit.Record(r.Context(), actor(r), "user.updated", strconv.FormatInt(id, 10), map[string]string{"role": in.Role})
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) setUserPassword(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	var in struct{ Password string }
	if !decode(w, r, &in) {
		return
	}
	if err := a.Auth.SetPassword(r.Context(), id, in.Password); err != nil {
		fail(w, 400, err.Error(), nil)
		return
	}
	a.Audit.Record(r.Context(), actor(r), "user.password_reset", strconv.FormatInt(id, 10), nil)
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) deleteUser(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	if id == auth.FromContext(r.Context()).ID {
		fail(w, 400, "cannot_delete_self", nil)
		return
	}
	if err := a.Auth.DeleteUser(r.Context(), id); err != nil {
		code := 400
		if errors.Is(err, auth.ErrNotFound) {
			code = 404
		}
		fail(w, code, err.Error(), nil)
		return
	}
	a.Audit.Record(r.Context(), actor(r), "user.deleted", strconv.FormatInt(id, 10), nil)
	w.WriteHeader(http.StatusNoContent)
}

// ---- mail ----

type mailOut struct {
	mail.Settings
	HasPassword bool `json:"has_password"`
}

func (a *API) getMail(w http.ResponseWriter, r *http.Request) {
	st, err := a.Mail.Load(r.Context())
	if err != nil {
		fail(w, 500, "internal", err)
		return
	}
	out := mailOut{Settings: st, HasPassword: st.Password != ""}
	out.Password = ""
	writeJSON(w, 200, out)
}

func (a *API) putMail(w http.ResponseWriter, r *http.Request) {
	var in mail.Settings
	if !decode(w, r, &in) {
		return
	}
	if err := a.Mail.Save(r.Context(), in); err != nil {
		fail(w, 400, err.Error(), nil)
		return
	}
	a.Audit.Record(r.Context(), actor(r), "settings.mail_saved", in.Host, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) testMail(w http.ResponseWriter, r *http.Request) {
	var in struct {
		To string `json:"to"`
	}
	if !decode(w, r, &in) {
		return
	}
	u := auth.FromContext(r.Context())
	to := in.To
	if to == "" {
		to = u.Email
	}
	msg := mail.Message{To: []string{to}, Subject: a.Cfg.Branding.ProductName + ": test email",
		Heading: "Your mail server works",
		Body:    []string{"This is a test email from " + a.Cfg.Branding.ProductName + ". Password resets and backup reports will be sent this way."}}
	if u.Language == "nl" {
		msg.Subject = a.Cfg.Branding.ProductName + ": testmail"
		msg.Heading = "Je mailserver werkt"
		msg.Body = []string{"Dit is een testmail van " + a.Cfg.Branding.ProductName + ". Wachtwoordherstel en backuprapporten worden op deze manier verstuurd."}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	if err := a.Mail.Send(ctx, msg); err != nil {
		fail(w, 502, "mail_failed", err)
		return
	}
	a.Audit.Record(r.Context(), actor(r), "settings.mail_tested", to, nil)
	w.WriteHeader(http.StatusNoContent)
}

// ---- backup ----

type backupOut struct {
	backup.Settings
	HasPassword   bool `json:"has_password"`
	HasKey        bool `json:"has_private_key"`
	HasPassphrase bool `json:"has_passphrase"`
}

func (a *API) getBackup(w http.ResponseWriter, r *http.Request) {
	st, err := a.Backup.Load(r.Context())
	if err != nil {
		fail(w, 500, "internal", err)
		return
	}
	out := backupOut{Settings: st, HasPassword: st.Target.Password != "", HasKey: st.Target.PrivateKey != "", HasPassphrase: st.Passphrase != ""}
	out.Target.Password, out.Target.PrivateKey, out.Passphrase = "", "", ""
	writeJSON(w, 200, out)
}

func (a *API) putBackup(w http.ResponseWriter, r *http.Request) {
	var in backup.Settings
	if !decode(w, r, &in) {
		return
	}
	switch in.Target.Type {
	case "ftp", "ftps-explicit", "ftps-implicit", "scp":
	default:
		fail(w, 400, "backup_target_type", nil)
		return
	}
	if !backup.ValidSchedule(in.FullSchedule) || !backup.ValidSchedule(in.IncrementalSchedule) {
		fail(w, 400, "backup_schedule", nil)
		return
	}
	if err := a.Backup.Save(r.Context(), in); err != nil {
		fail(w, 400, err.Error(), nil)
		return
	}
	// Whichever server runs the scheduler (the cluster leader) reloads it.
	if a.Bus != nil {
		a.Bus.Publish(cluster.EventBackupSettings, nil)
	} else if a.Scheduler != nil {
		a.Scheduler.Reload(r.Context())
	}
	a.Audit.Record(r.Context(), actor(r), "settings.backup_saved", in.Target.Label(), nil)
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) testBackup(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	hk, err := a.Backup.Test(ctx)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": "backup_target_failed", "detail": err.Error(), "host_key": hk})
		return
	}
	writeJSON(w, 200, map[string]string{"host_key": hk})
}

func (a *API) runBackup(w http.ResponseWriter, r *http.Request) {
	var in struct{ Kind string }
	if !decode(w, r, &in) {
		return
	}
	if in.Kind != "full" && in.Kind != "incremental" {
		fail(w, 400, "bad_request", nil)
		return
	}
	act := actor(r)
	background(func(ctx context.Context) {
		run, err := a.Backup.Run(ctx, in.Kind)
		a.RecordBackup(ctx, act, in.Kind, run, err)
	})
	w.WriteHeader(http.StatusAccepted)
}

// recordBackup writes the audit entry and mails admins when a backup fails.
func (a *API) RecordBackup(ctx context.Context, act audit.Actor, kind string, run *backup.Run, err error) {
	if errors.Is(err, backup.ErrBusy) {
		return
	}
	name := ""
	if run != nil {
		name = run.Name
	}
	if err != nil {
		a.Audit.Record(ctx, act, "backup.failed", name, map[string]string{"kind": kind, "error": err.Error()})
		a.notifyAdmins(ctx, "Backup failed", "Back-up mislukt", "The "+kind+" backup failed: "+err.Error(), "De "+kind+" back-up is mislukt: "+err.Error())
		return
	}
	a.Audit.Record(ctx, act, "backup.completed", name, map[string]string{"kind": run.Kind})
}

func (a *API) notifyAdmins(ctx context.Context, subjEN, subjNL, bodyEN, bodyNL string) {
	us, err := a.Auth.ListUsers(ctx)
	if err != nil {
		return
	}
	for _, u := range us {
		if !u.IsAdmin() {
			continue
		}
		m := mail.Message{To: []string{u.Email}, Subject: a.Cfg.Branding.ProductName + ": " + subjEN, Heading: subjEN, Body: []string{bodyEN}}
		if u.Language == "nl" {
			m = mail.Message{To: []string{u.Email}, Subject: a.Cfg.Branding.ProductName + ": " + subjNL, Heading: subjNL, Body: []string{bodyNL}}
		}
		a.Mail.Send(ctx, m) // best effort; mail may not be configured
	}
}

func (a *API) backupRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := a.Backup.Runs(r.Context(), 50)
	if err != nil {
		fail(w, 500, "internal", err)
		return
	}
	writeJSON(w, 200, runs)
}

func (a *API) remoteBackups(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	fs, err := a.Backup.ListRemote(ctx)
	if err != nil {
		fail(w, 502, "backup_target_failed", err)
		return
	}
	if fs == nil {
		fs = []backup.RemoteFile{}
	}
	writeJSON(w, 200, fs)
}

func (a *API) restoreBackup(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name    string `json:"name"`
		Confirm string `json:"confirm"`
	}
	if !decode(w, r, &in) {
		return
	}
	// The portal asks the admin to type RESTORE, because this replaces all current data.
	if in.Confirm != "RESTORE" {
		fail(w, 400, "restore_confirm", nil)
		return
	}
	a.restore.Lock()
	if a.restore.Running {
		a.restore.Unlock()
		fail(w, 409, "backup_busy", nil)
		return
	}
	a.restore.Running, a.restore.Name, a.restore.Error, a.restore.Done = true, in.Name, "", false
	a.restore.Unlock()
	act := actor(r)
	a.Audit.Record(r.Context(), act, "backup.restore_started", in.Name, nil)
	background(func(ctx context.Context) {
		err := a.Backup.Restore(ctx, in.Name)
		a.restore.Lock()
		a.restore.Running, a.restore.Done = false, true
		if err != nil {
			a.restore.Error = err.Error()
		}
		a.restore.Unlock()
		if err != nil {
			a.Audit.Record(ctx, act, "backup.restore_failed", in.Name, map[string]string{"error": err.Error()})
		} else {
			// The audit log itself was restored too; this entry marks the restore in the new state.
			a.Audit.Record(ctx, audit.Actor{IP: act.IP}, "backup.restored", in.Name, map[string]string{"by": act.Email})
		}
	})
	w.WriteHeader(http.StatusAccepted)
}

func (a *API) restoreStatus(w http.ResponseWriter, r *http.Request) {
	a.restore.Lock()
	defer a.restore.Unlock()
	writeJSON(w, 200, map[string]any{"running": a.restore.Running, "name": a.restore.Name, "error": a.restore.Error, "done": a.restore.Done})
}

// ---- system: version and updates ----

func (a *API) systemInfo(w http.ResponseWriter, r *http.Request) {
	st, err := a.Update.Settings(r.Context())
	if err != nil {
		fail(w, 500, "internal", err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"product_name":  a.Cfg.Branding.ProductName,
		"version":       version.Version,
		"commit":        version.Commit,
		"build_date":    version.BuildDate,
		"build_channel": version.Channel,
		"channel":       st.Channel,
		"roles":         a.Cfg.Roles,
		"last_check":    a.Update.Last(),
	})
}

func (a *API) checkUpdate(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	st, err := a.Update.Check(ctx)
	if err != nil {
		fail(w, 500, "internal", err)
		return
	}
	writeJSON(w, 200, st)
}

func (a *API) setChannel(w http.ResponseWriter, r *http.Request) {
	var in struct{ Channel string }
	if !decode(w, r, &in) {
		return
	}
	if err := a.Update.SetChannel(r.Context(), in.Channel); err != nil {
		fail(w, 400, err.Error(), nil)
		return
	}
	a.Audit.Record(r.Context(), actor(r), "system.channel_changed", in.Channel, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) applyUpdate(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	rel, err := a.Update.Apply(ctx)
	if err != nil {
		fail(w, 502, "update_failed", err)
		return
	}
	a.Audit.Record(r.Context(), actor(r), "system.updated", rel.Version, map[string]string{"from": version.Version})
	writeJSON(w, 200, map[string]any{"installed": rel.Version, "restarting": true})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	if a.Restart != nil {
		go func() {
			time.Sleep(time.Second)
			a.Restart()
		}()
	}
}
