// SPDX-License-Identifier: AGPL-3.0-only
// Quick support: connect to a customer who runs the portable QuickSupport exe.
import { FormEvent, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { api } from "../api";
import { Icon } from "../components/Icon";
import { Notice, PageHead, errorText, useLoad } from "../components/ui";

type QuickInfo = { available: boolean; download_path: string; download_url: string; https: boolean; version?: string; size?: number };
type StartResult = { session_id: string; device_id: number };

/** formatCode turns any input into "123 456 789" while typing. */
export function formatCode(v: string): string {
  const d = v.replace(/\D/g, "").slice(0, 9);
  return d.replace(/(\d{3})(?=\d)/g, "$1 ");
}

export function QuickSupport() {
  const { t } = useTranslation();
  const nav = useNavigate();
  const info = useLoad(() => api.get<QuickInfo>("/quick/info"));
  const [code, setCode] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<unknown>(null);
  const [copied, setCopied] = useState(false);

  const digits = code.replace(/\D/g, "");
  const link = info.data?.download_url ?? "";

  const connect = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setErr(null);
    try {
      const r = await api.post<StartResult>("/quick/connect", { code: digits });
      nav(`/session/${r.session_id}`);
    } catch (e) {
      setErr(e);
    } finally {
      setBusy(false);
    }
  };

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(link);
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    } catch {
      /* clipboard blocked: the link is still visible to select */
    }
  };

  return (
    <>
      <PageHead title={t("quick.title")} subtitle={t("quick.subtitle")} />
      <div className="grid2">
        <form className="card" onSubmit={connect}>
          <div className="card-head">
            <h2>{t("quick.connect_title")}</h2>
          </div>
          <p className="muted">{t("quick.connect_help")}</p>
          <div className="field">
            <label htmlFor="quick-code">{t("quick.code")}</label>
            <input
              id="quick-code"
              className="input quick-code"
              inputMode="numeric"
              autoComplete="off"
              autoFocus
              placeholder="123 456 789"
              value={code}
              onChange={(e) => setCode(formatCode(e.target.value))}
            />
          </div>
          {!!err && <Notice kind="error">{errorText(t, err)}</Notice>}
          <div className="row">
            <button className="btn btn-primary" disabled={digits.length !== 9 || busy}>
              <Icon name="quick" />
              {busy ? t("quick.connecting") : t("quick.connect")}
            </button>
          </div>
          <p className="muted small">{t("quick.consent_note")}</p>
        </form>

        <div className="card">
          <div className="card-head">
            <h2>{t("quick.download_title")}</h2>
            {info.data?.version && <span className="pill neutral">v{info.data.version}</span>}
          </div>
          <p className="muted">{t("quick.download_help")}</p>
          {info.data && !info.data.available && <Notice kind="warn">{t("quick.not_available")}</Notice>}
          {info.data && !info.data.https && <Notice kind="warn">{t("quick.needs_https")}</Notice>}
          {info.data?.available && (
            <>
              <div className="field">
                <label htmlFor="quick-link">{t("quick.link")}</label>
                <input id="quick-link" className="input" readOnly value={link} onFocus={(e) => e.target.select()} />
              </div>
              <div className="row">
                <button type="button" className="btn btn-secondary" onClick={copy}>
                  <Icon name="copy" />
                  {copied ? t("quick.copied") : t("quick.copy_link")}
                </button>
                <a className="btn btn-secondary" href={info.data.download_path}>
                  <Icon name="download" />
                  {t("quick.download")}
                </a>
              </div>
            </>
          )}
          {!!info.error && <Notice kind="error">{errorText(t, info.error)}</Notice>}
        </div>
      </div>

      <div className="card" style={{ marginTop: 14 }}>
        <h2>{t("quick.how_title")}</h2>
        <ol className="steps">
          <li>{t("quick.how_1")}</li>
          <li>{t("quick.how_2")}</li>
          <li>{t("quick.how_3")}</li>
          <li>{t("quick.how_4")}</li>
        </ol>
      </div>
    </>
  );
}
