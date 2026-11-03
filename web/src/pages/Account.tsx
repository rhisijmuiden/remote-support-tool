import { FormEvent, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../api";
import { useSession } from "../session";
import { Notice, PageHead, errorText } from "../components/ui";

export function Account() {
  const { t } = useTranslation();
  const { user, setUser } = useSession();
  const [f, setF] = useState({ current: "", new: "" });
  const [err, setErr] = useState("");
  const [done, setDone] = useState(false);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setErr("");
    try {
      await api.put("/me/password", f);
      setDone(true);
      setTimeout(() => setUser(null), 2500);
    } catch (e) {
      setErr(errorText(t, e));
    }
  };
  return (
    <>
      <PageHead title={t("account.title")} subtitle={`${user.name} · ${user.email}`} />
      <form className="card" style={{ maxWidth: 520 }} onSubmit={submit}>
        <h2>{t("account.change")}</h2>
        {err && <Notice kind="error">{err}</Notice>}
        {done && <Notice kind="ok">{t("account.changed")}</Notice>}
        <div className="field">
          <label htmlFor="pw-cur">{t("account.current")}</label>
          <input id="pw-cur" className="input" type="password" autoComplete="current-password" required value={f.current} onChange={(e) => setF({ ...f, current: e.target.value })} />
        </div>
        <div className="field">
          <label htmlFor="pw-new">{t("account.new")}</label>
          <input id="pw-new" className="input" type="password" autoComplete="new-password" minLength={12} required value={f.new} onChange={(e) => setF({ ...f, new: e.target.value })} />
          <span className="hint">{t("auth.password_hint")}</span>
        </div>
        <div>
          <button className="btn btn-primary">{t("account.change")}</button>
        </div>
      </form>
    </>
  );
}
