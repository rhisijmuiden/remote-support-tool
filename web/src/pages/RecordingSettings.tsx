import { FormEvent, useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { api, RecordingPolicy } from "../api";
import { Notice, PageHead, errorText } from "../components/ui";

export function RecordingSettings() {
  const { t } = useTranslation();
  const [f, setF] = useState<RecordingPolicy | null>(null);
  const [msg, setMsg] = useState<{ kind: "ok" | "error"; text: string } | null>(null);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    api.get<RecordingPolicy>("/settings/recording").then(setF, (e) => setMsg({ kind: "error", text: errorText(t, e) }));
  }, [t]);

  const head = <PageHead title={t("recset.title")} subtitle={t("recset.subtitle")} />;
  if (!f) return (
    <>
      {head}
      {msg && <Notice kind={msg.kind}>{msg.text}</Notice>}
    </>
  );
  const set = (p: Partial<RecordingPolicy>) => setF({ ...f, ...p });

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setMsg(null);
    try {
      await api.put("/settings/recording", f);
      setMsg({ kind: "ok", text: t("common.saved") });
    } catch (err) {
      setMsg({ kind: "error", text: errorText(t, err) });
    }
    setBusy(false);
  };

  return (
    <>
      {head}
      {msg && <Notice kind={msg.kind}>{msg.text}</Notice>}
      <form className="card" onSubmit={save}>
        <h2>{t("recset.mode")}</h2>
        <div className="choices" role="radiogroup" aria-label={t("recset.mode")}>
          {(["off", "optional", "always"] as const).map((m) => (
            <label key={m} className={"choice" + (f.mode === m ? " on" : "")}>
              <input type="radio" name="mode" checked={f.mode === m} onChange={() => set({ mode: m })} />
              <strong>{t("recset.mode_" + m)}</strong>
              <span className="hint">{t("recset.mode_" + m + "_hint")}</span>
            </label>
          ))}
        </div>
        <div className="grid2">
          <div className="field">
            <label htmlFor="retention">{t("recset.retention")}</label>
            <input id="retention" className="input" type="number" min={0} max={3650} value={f.retention_days} onChange={(e) => set({ retention_days: Number(e.target.value) })} />
            <span className="hint">{t("recset.retention_hint")}</span>
          </div>
          <div className="field">
            <label htmlFor="share-days">{t("recset.share_days")}</label>
            <input id="share-days" className="input" type="number" min={1} max={90} value={f.share_days} onChange={(e) => set({ share_days: Number(e.target.value) })} />
            <span className="hint">{t("recset.share_days_hint")}</span>
          </div>
        </div>
        <div className="row">
          <button className="btn btn-primary" disabled={busy}>
            {t("common.save")}
          </button>
        </div>
      </form>
      <div className="card">
        <h2>{t("recset.how_title")}</h2>
        <ul className="bullets">
          <li>{t("recset.how_banner")}</li>
          <li>{t("recset.how_encrypted")}</li>
          <li>{t("recset.how_key")}</li>
          <li>{t("recset.how_backup")}</li>
          <li>{t("recset.how_privacy")}</li>
        </ul>
      </div>
    </>
  );
}
