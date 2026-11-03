import { FormEvent, useState } from "react";
import { useTranslation } from "react-i18next";
import { api, User } from "../api";
import { useSession } from "../session";
import { Icon } from "../components/Icon";
import { Dialog, Notice, PageHead, errorText, formatDate, useLoad } from "../components/ui";

type Edit = { mode: "new" } | { mode: "edit"; user: User } | { mode: "password"; user: User } | { mode: "delete"; user: User };

export function Users() {
  const { t, i18n } = useTranslation();
  const { user: me } = useSession();
  const { data, error, reload } = useLoad(() => api.get<User[]>("/users"));
  const [edit, setEdit] = useState<Edit | null>(null);
  return (
    <>
      <PageHead
        title={t("users.title")}
        subtitle={t("users.subtitle")}
        actions={
          <button className="btn btn-primary" onClick={() => setEdit({ mode: "new" })}>
            {t("users.add")}
          </button>
        }
      />
      {!!error && <Notice kind="error">{errorText(t, error)}</Notice>}
      <div className="card">
        <p className="hint">{t("users.role_help")}</p>
        <div className="tbl-wrap">
          <table>
            <thead>
              <tr>
                <th>{t("users.name")}</th>
                <th>{t("users.role")}</th>
                <th>{t("users.language")}</th>
                <th>{t("users.last_login")}</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {(data ?? []).map((u) => (
                <tr key={u.id}>
                  <td>
                    {u.name} {u.id === me.id && <span className="muted">({t("users.you")})</span>}
                    <span className="sub">{u.email}</span>
                  </td>
                  <td>
                    <span className={"pill " + (u.role === "admin" ? "warn" : "neutral")}>{t("users." + u.role)}</span>
                  </td>
                  <td>{u.language.toUpperCase()}</td>
                  <td>{u.last_login_at ? formatDate(u.last_login_at, i18n.language) : t("common.never")}</td>
                  <td style={{ textAlign: "right", whiteSpace: "nowrap" }}>
                    <button className="btn-icon" title={t("common.edit")} aria-label={t("common.edit")} onClick={() => setEdit({ mode: "edit", user: u })}>
                      <Icon name="edit" />
                    </button>
                    <button className="btn-icon" title={t("users.reset_password")} aria-label={t("users.reset_password")} onClick={() => setEdit({ mode: "password", user: u })}>
                      <Icon name="key" />
                    </button>
                    {u.id !== me.id && (
                      <button className="btn-icon" title={t("common.delete")} aria-label={t("common.delete")} onClick={() => setEdit({ mode: "delete", user: u })}>
                        <Icon name="trash" />
                      </button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </div>
      {edit && <UserDialog edit={edit} onClose={() => setEdit(null)} onDone={() => (setEdit(null), reload())} />}
    </>
  );
}

function UserDialog({ edit, onClose, onDone }: { edit: Edit; onClose: () => void; onDone: () => void }) {
  const { t } = useTranslation();
  const u = edit.mode === "new" ? null : edit.user;
  const [f, setF] = useState({ name: u?.name ?? "", email: u?.email ?? "", role: u?.role ?? "technician", language: u?.language ?? "en", password: "" });
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setErr("");
    try {
      if (edit.mode === "new") await api.post("/users", f);
      if (edit.mode === "edit") await api.put(`/users/${edit.user.id}`, { name: f.name, role: f.role, language: f.language });
      if (edit.mode === "password") await api.put(`/users/${edit.user.id}/password`, { password: f.password });
      if (edit.mode === "delete") await api.del(`/users/${edit.user.id}`);
      onDone();
    } catch (e) {
      setErr(errorText(t, e));
      setBusy(false);
    }
  };

  const title =
    edit.mode === "new" ? t("users.add") : edit.mode === "password" ? t("users.reset_password") : edit.mode === "delete" ? t("common.delete") : t("common.edit");

  return (
    <Dialog title={title} onClose={onClose}>
      <form onSubmit={submit} style={{ display: "grid", gap: 14 }}>
        {err && <Notice kind="error">{err}</Notice>}
        {edit.mode === "delete" && <p>{t("users.confirm_delete", { name: edit.user.name })}</p>}
        {(edit.mode === "new" || edit.mode === "edit") && (
          <>
            <div className="field">
              <label htmlFor="u-name">{t("users.name")}</label>
              <input id="u-name" className="input" required value={f.name} onChange={(e) => setF({ ...f, name: e.target.value })} />
            </div>
            {edit.mode === "new" && (
              <div className="field">
                <label htmlFor="u-email">{t("users.email")}</label>
                <input id="u-email" className="input" type="email" required value={f.email} onChange={(e) => setF({ ...f, email: e.target.value })} />
              </div>
            )}
            <div className="grid2">
              <div className="field">
                <label htmlFor="u-role">{t("users.role")}</label>
                <select id="u-role" className="input" value={f.role} onChange={(e) => setF({ ...f, role: e.target.value as User["role"] })}>
                  <option value="technician">{t("users.technician")}</option>
                  <option value="admin">{t("users.admin")}</option>
                </select>
              </div>
              <div className="field">
                <label htmlFor="u-lang">{t("users.language")}</label>
                <select id="u-lang" className="input" value={f.language} onChange={(e) => setF({ ...f, language: e.target.value as User["language"] })}>
                  <option value="en">English</option>
                  <option value="nl">Nederlands</option>
                </select>
              </div>
            </div>
          </>
        )}
        {(edit.mode === "new" || edit.mode === "password") && (
          <div className="field">
            <label htmlFor="u-pw">{edit.mode === "new" ? t("users.password") : t("users.new_password")}</label>
            <input id="u-pw" className="input" type="password" autoComplete="new-password" minLength={12} required value={f.password} onChange={(e) => setF({ ...f, password: e.target.value })} />
            <span className="hint">{t("auth.password_hint")}</span>
          </div>
        )}
        <div className="row">
          <button className={"btn " + (edit.mode === "delete" ? "btn-danger" : "btn-primary")} disabled={busy}>
            {edit.mode === "delete" ? t("common.delete") : t("common.save")}
          </button>
          <button type="button" className="btn btn-secondary" onClick={onClose}>
            {t("common.cancel")}
          </button>
        </div>
      </form>
    </Dialog>
  );
}
