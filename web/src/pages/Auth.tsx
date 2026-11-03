import { FormEvent, ReactNode, useState } from "react";
import { useTranslation } from "react-i18next";
import { api, Meta, User } from "../api";
import { setLanguage } from "../i18n";
import { Logo } from "../components/Logo";
import { Notice, errorText } from "../components/ui";

function AuthFrame({ children }: { children: ReactNode }) {
  const { t, i18n } = useTranslation();
  return (
    <div className="auth">
      <div className="auth-art">
        <Logo className="logo" />
        <div>
          <h1>{t("auth.tagline")}</h1>
          <p>{t("auth.pitch")}</p>
        </div>
        <span className="seg" role="group" aria-label="Language" style={{ alignSelf: "flex-start" }}>
          <button aria-pressed={i18n.language === "en"} onClick={() => setLanguage("en")}>
            EN
          </button>
          <button aria-pressed={i18n.language === "nl"} onClick={() => setLanguage("nl")}>
            NL
          </button>
        </span>
      </div>
      <div className="auth-form">{children}</div>
    </div>
  );
}

export function Login({ onDone }: { meta: Meta; onDone: (u: User) => void }) {
  const { t } = useTranslation();
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setErr("");
    try {
      onDone(await api.post<User>("/login", { email, password }));
    } catch (e) {
      setErr(errorText(t, e));
      setBusy(false);
    }
  };
  return (
    <AuthFrame>
      <form onSubmit={submit}>
        <h1>{t("auth.signin_title")}</h1>
        {err && <Notice kind="error">{err}</Notice>}
        <div className="field">
          <label htmlFor="email">{t("auth.email")}</label>
          <input id="email" className="input" type="email" autoComplete="username" required value={email} onChange={(e) => setEmail(e.target.value)} />
        </div>
        <div className="field">
          <label htmlFor="password">{t("auth.password")}</label>
          <input id="password" className="input" type="password" autoComplete="current-password" required value={password} onChange={(e) => setPassword(e.target.value)} />
        </div>
        <button className="btn btn-primary" disabled={busy}>
          {t("auth.signin")}
        </button>
      </form>
    </AuthFrame>
  );
}

export function Setup({ onDone }: { meta: Meta; onDone: (u: User) => void }) {
  const { t, i18n } = useTranslation();
  const [f, setF] = useState({ name: "", email: "", password: "" });
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setErr("");
    try {
      onDone(await api.post<User>("/setup", { ...f, language: i18n.language }));
    } catch (e) {
      setErr(errorText(t, e));
      setBusy(false);
    }
  };
  return (
    <AuthFrame>
      <form onSubmit={submit}>
        <h1>{t("auth.setup_title")}</h1>
        <p className="muted">{t("auth.setup_intro")}</p>
        {err && <Notice kind="error">{err}</Notice>}
        <div className="field">
          <label htmlFor="name">{t("auth.name")}</label>
          <input id="name" className="input" required value={f.name} onChange={(e) => setF({ ...f, name: e.target.value })} />
        </div>
        <div className="field">
          <label htmlFor="email">{t("auth.email")}</label>
          <input id="email" className="input" type="email" autoComplete="username" required value={f.email} onChange={(e) => setF({ ...f, email: e.target.value })} />
        </div>
        <div className="field">
          <label htmlFor="password">{t("auth.password")}</label>
          <input id="password" className="input" type="password" autoComplete="new-password" minLength={12} required value={f.password} onChange={(e) => setF({ ...f, password: e.target.value })} />
          <span className="hint">{t("auth.password_hint")}</span>
        </div>
        <button className="btn btn-primary" disabled={busy}>
          {t("auth.create")}
        </button>
      </form>
    </AuthFrame>
  );
}
