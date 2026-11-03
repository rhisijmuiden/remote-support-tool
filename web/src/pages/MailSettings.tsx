import { FormEvent, useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../api";
import { useSession } from "../session";
import { Notice, PageHead, errorText } from "../components/ui";

type Mail = {
  enabled: boolean;
  host: string;
  port: number;
  security: "starttls" | "tls" | "none";
  auth: string;
  username: string;
  password?: string;
  from_address: string;
  from_name: string;
  reply_to: string;
  helo: string;
  min_tls: "1.2" | "1.3";
  has_password?: boolean;
};

const defaultPort = { starttls: 587, tls: 465, none: 25 };

export function MailSettings() {
  const { t } = useTranslation();
  const { user } = useSession();
  const [f, setF] = useState<Mail | null>(null);
  const [dirty, setDirty] = useState(false);
  const [msg, setMsg] = useState<{ kind: "ok" | "error"; text: string } | null>(null);
  const [busy, setBusy] = useState(false);
  const [to, setTo] = useState(user.email);

  useEffect(() => {
    api.get<Mail>("/settings/mail").then(setF, (e) => setMsg({ kind: "error", text: errorText(t, e) }));
  }, [t]);

  if (!f) return <PageHead title={t("mail.title")} subtitle={t("mail.subtitle")} />;
  const set = (p: Partial<Mail>) => (setF({ ...f, ...p }), setDirty(true));

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setMsg(null);
    try {
      await api.put("/settings/mail", f);
      setF({ ...f, password: "", has_password: f.has_password || !!f.password });
      setDirty(false);
      setMsg({ kind: "ok", text: t("common.saved") });
    } catch (e) {
      setMsg({ kind: "error", text: errorText(t, e) });
    }
    setBusy(false);
  };

  const test = async () => {
    setBusy(true);
    setMsg(null);
    try {
      await api.post("/settings/mail/test", { to });
      setMsg({ kind: "ok", text: t("mail.test_sent", { to }) });
    } catch (e) {
      setMsg({ kind: "error", text: errorText(t, e) });
    }
    setBusy(false);
  };

  return (
    <>
      <PageHead title={t("mail.title")} subtitle={t("mail.subtitle")} />
      {msg && <Notice kind={msg.kind}>{msg.text}</Notice>}
      <form className="card" onSubmit={save}>
        <label className="check">
          <input id="m-enabled" type="checkbox" checked={f.enabled} onChange={(e) => set({ enabled: e.target.checked })} />
          <span>{t("mail.enabled")}</span>
        </label>
        <fieldset disabled={!f.enabled} style={{ border: 0, padding: 0, margin: 0, display: "grid", gap: 14, opacity: f.enabled ? 1 : 0.55 }}>
          <div className="grid3">
            <div className="field" style={{ gridColumn: "span 2" }}>
              <label htmlFor="m-host">{t("mail.host")}</label>
              <input id="m-host" className="input" placeholder="smtp.example.com" value={f.host} onChange={(e) => set({ host: e.target.value })} />
            </div>
            <div className="field">
              <label htmlFor="m-port">{t("mail.port")}</label>
              <input id="m-port" className="input" type="number" min={1} max={65535} value={f.port} onChange={(e) => set({ port: Number(e.target.value) })} />
            </div>
          </div>
          <div className="grid2">
            <div className="field">
              <label htmlFor="m-sec">{t("mail.security")}</label>
              <select id="m-sec" className="input" value={f.security} onChange={(e) => {
                const s = e.target.value as Mail["security"];
                set({ security: s, port: Object.values(defaultPort).includes(f.port) ? defaultPort[s] : f.port });
              }}>
                <option value="starttls">{t("mail.sec_starttls")}</option>
                <option value="tls">{t("mail.sec_tls")}</option>
                <option value="none">{t("mail.sec_none")}</option>
              </select>
            </div>
            <div className="field">
              <label htmlFor="m-auth">{t("mail.auth")}</label>
              <select id="m-auth" className="input" value={f.auth} onChange={(e) => set({ auth: e.target.value })}>
                <option value="auto">{t("mail.auth_auto")}</option>
                <option value="scram-sha-256">SCRAM-SHA-256</option>
                <option value="plain">PLAIN</option>
                <option value="login">LOGIN</option>
                <option value="cram-md5">CRAM-MD5</option>
                <option value="xoauth2">XOAUTH2</option>
                <option value="none">{t("mail.auth_none")}</option>
              </select>
            </div>
          </div>
          {f.auth !== "none" && (
            <div className="grid2">
              <div className="field">
                <label htmlFor="m-user">{t("mail.username")}</label>
                <input id="m-user" className="input" autoComplete="off" value={f.username} onChange={(e) => set({ username: e.target.value })} />
              </div>
              <div className="field">
                <label htmlFor="m-pass">{t("mail.password")}</label>
                <input id="m-pass" className="input" type="password" autoComplete="new-password" placeholder={f.has_password ? "••••••••" : ""} value={f.password ?? ""} onChange={(e) => set({ password: e.target.value })} />
                <span className="hint">{f.has_password ? t("common.keep_secret") : ""}</span>
              </div>
            </div>
          )}
          <div className="grid3">
            <div className="field">
              <label htmlFor="m-fromname">{t("mail.from_name")}</label>
              <input id="m-fromname" className="input" value={f.from_name} onChange={(e) => set({ from_name: e.target.value })} />
            </div>
            <div className="field">
              <label htmlFor="m-from">{t("mail.from_address")}</label>
              <input id="m-from" className="input" type="email" placeholder="support@example.com" value={f.from_address} onChange={(e) => set({ from_address: e.target.value })} />
            </div>
            <div className="field">
              <label htmlFor="m-reply">{t("mail.reply_to")}</label>
              <input id="m-reply" className="input" type="email" value={f.reply_to} onChange={(e) => set({ reply_to: e.target.value })} />
            </div>
          </div>
          <details>
            <summary className="label" style={{ cursor: "pointer" }}>{t("mail.advanced")}</summary>
            <div className="grid2" style={{ marginTop: 12 }}>
              <div className="field">
                <label htmlFor="m-tls">{t("mail.min_tls")}</label>
                <select id="m-tls" className="input" value={f.min_tls} onChange={(e) => set({ min_tls: e.target.value as Mail["min_tls"] })}>
                  <option value="1.2">TLS 1.2</option>
                  <option value="1.3">TLS 1.3</option>
                </select>
              </div>
              <div className="field">
                <label htmlFor="m-helo">{t("mail.helo")}</label>
                <input id="m-helo" className="input" value={f.helo} onChange={(e) => set({ helo: e.target.value })} />
                <span className="hint">{t("mail.helo_hint")}</span>
              </div>
            </div>
          </details>
        </fieldset>
        <div>
          <button className="btn btn-primary" disabled={busy || !dirty}>
            {t("common.save")}
          </button>
        </div>
      </form>
      <div className="card">
        <h2>{t("mail.test")}</h2>
        <div className="row">
          <input id="m-test-to" className="input" style={{ maxWidth: 320 }} type="email" aria-label={t("mail.test_to")} value={to} onChange={(e) => setTo(e.target.value)} />
          <button className="btn btn-secondary" onClick={test} disabled={busy || dirty || !f.enabled}>
            {t("mail.test")}
          </button>
        </div>
        {dirty && <span className="hint">{t("mail.save_first")}</span>}
      </div>
    </>
  );
}
