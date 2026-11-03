import { useEffect, useState } from "react";
import { useParams } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { duration } from "../api";
import { setLanguage } from "../i18n";
import { Icon } from "../components/Icon";
import { Logo } from "../components/Logo";
import { formatBytes, formatDate } from "../components/ui";

type Shared = {
  company_name: string;
  product_name: string;
  device_name: string | null;
  customer_name: string | null;
  technician_name: string | null;
  started_at: string;
  ended_at: string | null;
  size_bytes: number;
  complete: boolean;
  expires_at: string;
  note: string | null;
};

// The page a customer opens from a share link. It needs no account.
export function SharedRecording() {
  const { t, i18n } = useTranslation();
  const { token } = useParams();
  const [data, setData] = useState<Shared | null>(null);
  const [gone, setGone] = useState(false);

  useEffect(() => {
    fetch(`/api/public/recordings/${encodeURIComponent(token ?? "")}`, { credentials: "omit" })
      .then((r) => (r.ok ? r.json() : Promise.reject(r)))
      .then((d: Shared) => {
        setData(d);
        document.title = `${d.product_name} · ${t("shared.title")}`;
      })
      .catch(() => setGone(true));
  }, [token, t]);

  const lang = i18n.language;
  const video = `/api/public/recordings/${encodeURIComponent(token ?? "")}/video`;
  return (
    <div className="shared">
      <header>
        <span className="brand" style={{ padding: 0 }}>
          <Logo />
          <small>Remote</small>
        </span>
        <span className="seg" role="group" aria-label="Language">
          <button aria-pressed={lang === "en"} onClick={() => setLanguage("en")}>
            EN
          </button>
          <button aria-pressed={lang === "nl"} onClick={() => setLanguage("nl")}>
            NL
          </button>
        </span>
      </header>
      <main>
        {gone ? (
          <div className="card empty">
            <Icon name="link" />
            <strong>{t("shared.gone_title")}</strong>
            <p>{t("shared.gone")}</p>
          </div>
        ) : !data ? (
          <div className="empty">{t("common.loading")}</div>
        ) : (
          <>
            <div>
              <h1>{t("shared.heading", { company: data.company_name })}</h1>
              <p className="muted">
                {data.device_name ?? ""} · {formatDate(data.started_at, lang)} · {duration(data)}
              </p>
            </div>
            {data.note && <div className="notice">{data.note}</div>}
            <div className="card">
              <video className="player" controls preload="metadata" src={video} />
              <div className="row spread">
                <span className="hint">
                  {t("shared.meta", { technician: data.technician_name ?? data.company_name, size: formatBytes(data.size_bytes) })}
                  {!data.complete && " " + t("session.recording_incomplete")}
                </span>
                <a className="btn btn-secondary btn-sm" href={video + "?download=1"}>
                  <Icon name="download" />
                  {t("session.download")}
                </a>
              </div>
            </div>
            <p className="hint">{t("shared.expires", { date: formatDate(data.expires_at, lang) })}</p>
          </>
        )}
      </main>
    </div>
  );
}
