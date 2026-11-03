// SPDX-License-Identifier: AGPL-3.0-only
import { FormEvent, useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { api, CodesignStatus } from "../api";
import { Notice, PageHead, errorText, formatDate } from "../components/ui";

function toBase64(f: File): Promise<string> {
  return new Promise((res, rej) => {
    const r = new FileReader();
    r.onload = () => res(String(r.result).split(",")[1] ?? "");
    r.onerror = rej;
    r.readAsDataURL(f);
  });
}

export function CodeSigning() {
  const { t, i18n } = useTranslation();
  const [st, setSt] = useState<CodesignStatus | null>(null);
  const [mode, setMode] = useState<CodesignStatus["mode"]>("");
  const [pfx, setPfx] = useState<File | null>(null);
  const [password, setPassword] = useState("");
  const [ts, setTs] = useState("");
  const [msg, setMsg] = useState<{ kind: "ok" | "error"; text: string } | null>(null);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    api.get<CodesignStatus>("/settings/codesign").then(
      (s) => (setSt(s), setMode(s.mode), setTs(s.timestamp_url)),
      (e) => setMsg({ kind: "error", text: errorText(t, e) }),
    );
  }, [t]);

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setMsg(null);
    try {
      const s = await api.put<CodesignStatus>("/settings/codesign", { mode, pfx_base64: pfx ? await toBase64(pfx) : "", password, timestamp_url: ts });
      setSt(s);
      setPfx(null);
      setPassword("");
      setMsg({ kind: "ok", text: t("common.saved") });
    } catch (e) {
      setMsg({ kind: "error", text: errorText(t, e) });
    }
    setBusy(false);
  };

  return (
    <>
      <PageHead title={t("codesign.title")} subtitle={t("codesign.subtitle")} />
      {msg && <Notice kind={msg.kind}>{msg.text}</Notice>}
      {st && (
        <div className="card">
          <div className="card-head">
            <h2>{t("codesign.status")}</h2>
            {st.ready ? <span className="pill ok">{t("codesign.ready")}</span> : st.mode ? <span className="pill off">{t("codesign.not_ready")}</span> : <span className="pill neutral">{t("codesign.off")}</span>}
          </div>
          <dl className="kv">
            <dt>{t("codesign.mode")}</dt>
            <dd>{t("codesign.mode_" + (st.mode || "off"))}</dd>
            {st.mode === "pfx" && (
              <>
                <dt>{t("codesign.subject")}</dt>
                <dd>{st.subject}</dd>
                <dt>{t("codesign.issuer")}</dt>
                <dd>{st.issuer}</dd>
                <dt>{t("codesign.expires")}</dt>
                <dd>{formatDate(st.not_after, i18n.language)}</dd>
                <dt>{t("codesign.thumbprint")}</dt>
                <dd className="mono">{st.thumbprint}</dd>
              </>
            )}
            {st.mode === "command" && (
              <>
                <dt>{t("codesign.command")}</dt>
                <dd className="mono">{st.command}</dd>
              </>
            )}
            <dt>{t("codesign.tool")}</dt>
            <dd>{st.tool || <span className="pill off">{t("codesign.tool_missing")}</span>}</dd>
          </dl>
          {!st.mode && <Notice kind="warn">{t("codesign.unsigned_warning")}</Notice>}
        </div>
      )}
      {st && (
        <form className="card" onSubmit={save}>
          <h2>{t("codesign.settings")}</h2>
          <div className="seg" role="group" aria-label={t("codesign.mode")}>
            <button type="button" aria-pressed={mode === ""} onClick={() => setMode("")}>
              {t("codesign.mode_off")}
            </button>
            <button type="button" aria-pressed={mode === "pfx"} onClick={() => setMode("pfx")}>
              {t("codesign.mode_pfx")}
            </button>
            <button type="button" aria-pressed={mode === "command"} disabled={!st.command} onClick={() => setMode("command")} title={st.command ? "" : t("codesign.command_missing")}>
              {t("codesign.mode_command")}
            </button>
          </div>
          {mode === "pfx" && (
            <div className="grid2">
              <div className="field">
                <label htmlFor="c-pfx">{t("codesign.pfx")}</label>
                <input id="c-pfx" className="input" type="file" accept=".pfx,.p12" onChange={(e) => setPfx(e.target.files?.[0] ?? null)} />
                {st.has_pfx && <span className="hint">{t("codesign.pfx_keep")}</span>}
              </div>
              <div className="field">
                <label htmlFor="c-pw">{t("codesign.password")}</label>
                <input id="c-pw" className="input" type="password" autoComplete="new-password" value={password} placeholder={st.has_pfx ? "••••••••" : ""} onChange={(e) => setPassword(e.target.value)} />
              </div>
            </div>
          )}
          {mode === "command" && <Notice>{t("codesign.command_hint")}</Notice>}
          {mode !== "" && (
            <div className="field" style={{ maxWidth: 420 }}>
              <label htmlFor="c-ts">{t("codesign.timestamp")}</label>
              <input id="c-ts" className="input mono" value={ts} onChange={(e) => setTs(e.target.value)} />
              <span className="hint">{t("codesign.timestamp_hint")}</span>
            </div>
          )}
          <span className="hint">{t("codesign.hsm_hint")}</span>
          <div className="row">
            <button className="btn btn-primary" disabled={busy || (mode === "pfx" && !st.has_pfx && !pfx)}>
              {t("common.save")}
            </button>
          </div>
        </form>
      )}
    </>
  );
}
