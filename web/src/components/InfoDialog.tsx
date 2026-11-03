import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { api, UpdateStatus } from "../api";
import { useSession } from "../session";
import { Dialog, Notice, errorText, formatDate } from "./ui";

type Info = {
  product_name: string;
  version: string;
  commit: string;
  build_date: string;
  build_channel: string;
  channel: "production" | "test";
  roles: string[];
  last_check: UpdateStatus | null;
};

const SOURCE_URL = "https://github.com/rhisijmuiden/remote-support-tool";

/** Info: version, update channel (production/test), update check and install. */
export function InfoDialog({ onClose }: { onClose: () => void }) {
  const { t, i18n } = useTranslation();
  const { user, meta } = useSession();
  const admin = user.role === "admin";
  const [info, setInfo] = useState<Info | null>(null);
  const [status, setStatus] = useState<UpdateStatus | null>(null);
  const [busy, setBusy] = useState<"" | "check" | "install" | "channel">("");
  const [err, setErr] = useState("");
  const [installed, setInstalled] = useState("");

  useEffect(() => {
    api.get<Info>("/system/info").then((i) => {
      setInfo(i);
      setStatus(i.last_check);
    });
  }, []);

  const check = async () => {
    setBusy("check");
    setErr("");
    try {
      setStatus(await api.post<UpdateStatus>("/system/check"));
    } catch (e) {
      setErr(errorText(t, e));
    }
    setBusy("");
  };

  const setChannel = async (ch: "production" | "test") => {
    if (!info || ch === info.channel) return;
    setBusy("channel");
    setErr("");
    try {
      await api.put("/system/channel", { channel: ch });
      setInfo({ ...info, channel: ch });
      setStatus(await api.post<UpdateStatus>("/system/check"));
    } catch (e) {
      setErr(errorText(t, e));
    }
    setBusy("");
  };

  const install = async () => {
    setBusy("install");
    setErr("");
    try {
      const r = await api.post<{ installed: string }>("/system/update");
      setInstalled(r.installed);
      // Wait for the server to come back, then reload.
      const wait = async () => {
        for (let i = 0; i < 60; i++) {
          await new Promise((r) => setTimeout(r, 2000));
          try {
            const m = await api.get<{ version: string }>("/meta");
            if (m.version === r.installed) return location.reload();
          } catch {
            /* still restarting */
          }
        }
      };
      wait();
    } catch (e) {
      setErr(errorText(t, e));
      setBusy("");
    }
  };

  const chName = (c: string) => t("info." + (c === "test" ? "test" : "production"));

  return (
    <Dialog title={t("info.title", { product: meta.product_name })} onClose={onClose}>
      {!info ? (
        <p className="muted">{t("common.loading")}</p>
      ) : (
        <>
          <dl className="kv">
            <dt>{t("info.version")}</dt>
            <dd>
              <strong className="mono">{info.version}</strong> <span className="pill neutral">{chName(info.build_channel)}</span>
            </dd>
            <dt>{t("info.commit")}</dt>
            <dd className="mono">{info.commit.slice(0, 12)}</dd>
            <dt>{t("info.built")}</dt>
            <dd>{info.build_date === "unknown" ? "-" : formatDate(info.build_date, i18n.language)}</dd>
            <dt>{t("info.roles")}</dt>
            <dd className="mono">{info.roles.join(", ")}</dd>
            <dt>{t("info.source")}</dt>
            <dd>
              <a href={SOURCE_URL} target="_blank" rel="noreferrer">
                github.com/rhisijmuiden/remote-support-tool
              </a>{" "}
              <span className="muted">(AGPL-3.0)</span>
            </dd>
          </dl>

          <div className="field">
            <span className="label">{t("info.channel")}</span>
            <span className="seg" role="group" aria-label={t("info.channel")}>
              {(["production", "test"] as const).map((c) => (
                <button key={c} aria-pressed={info.channel === c} disabled={!admin || busy !== ""} onClick={() => setChannel(c)}>
                  {chName(c)}
                </button>
              ))}
            </span>
            <span className="hint">{t("info.channel_hint")}</span>
          </div>

          {status && !installed && (
            <>
              {status.check_error ? (
                <Notice kind="error">{t("info.check_error", { error: status.check_error })}</Notice>
              ) : status.available && status.latest ? (
                <Notice kind={status.is_downgrade ? "warn" : "ok"}>
                  {t(status.is_downgrade ? "info.downgrade" : "info.available", { version: status.latest.version })}
                </Notice>
              ) : (
                <Notice kind="ok">{t("info.up_to_date", { channel: chName(status.channel).toLowerCase() })}</Notice>
              )}
              {status.available && status.latest?.notes && (
                <div className="field">
                  <span className="label">{t("info.release_notes")}</span>
                  <div className="release-notes">{status.latest.notes}</div>
                </div>
              )}
            </>
          )}
          {installed && <Notice kind="ok">{t("info.installed", { version: installed })}</Notice>}
          {err && <Notice kind="error">{err}</Notice>}
          {!admin && <p className="hint">{t("info.admin_only")}</p>}

          <div className="row">
            {admin && status?.available && status.latest && !installed && (
              <button className="btn btn-primary" onClick={install} disabled={busy !== ""}>
                {busy === "install" ? t("info.installing") : t("info.install", { version: status.latest.version })}
              </button>
            )}
            {admin && (
              <button className="btn btn-secondary" onClick={check} disabled={busy !== ""}>
                {busy === "check" ? t("info.checking") : t("info.check")}
              </button>
            )}
          </div>
        </>
      )}
    </Dialog>
  );
}
