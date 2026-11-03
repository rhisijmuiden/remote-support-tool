import { useState } from "react";
import { Link, useParams } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { api, duration, hasRecording, RemoteSession, SessionEvent, Share } from "../api";
import { Icon } from "../components/Icon";
import { Dialog, Empty, Notice, PageHead, errorText, formatBytes, formatDate, useLoad } from "../components/ui";
import { useSession } from "../session";
import { RecordingPill } from "./Sessions";

type Detail = { session: RemoteSession; events: SessionEvent[]; shares: Share[]; share_days: number };

function eventText(t: (k: string, o?: Record<string, unknown>) => string, e: SessionEvent) {
  const d = (e.details ?? {}) as Record<string, unknown>;
  return t("timeline." + e.kind.replace(/\./g, "_"), { ...d, defaultValue: e.kind });
}

export function SessionDetail() {
  const { t, i18n } = useTranslation();
  const { id } = useParams();
  const { user } = useSession();
  const { data, error, reload } = useLoad(() => api.get<Detail>(`/sessions/${id}`), [id]);
  const [sharing, setSharing] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [actionError, setActionError] = useState<unknown>(null);

  if (error) {
    return (
      <>
        <BackLink />
        <Notice kind="error">{errorText(t, error)}</Notice>
      </>
    );
  }
  if (!data) return <div className="empty">{t("common.loading")}</div>;
  const s = data.session;
  const rec = hasRecording(s);
  const lang = i18n.language;

  const revoke = async (sh: Share) => {
    try {
      await api.del(`/sessions/${s.id}/shares/${sh.id}`);
      reload();
    } catch (e) {
      setActionError(e);
    }
  };
  const del = async () => {
    try {
      await api.del(`/sessions/${s.id}/recording`);
      setConfirmDelete(false);
      reload();
    } catch (e) {
      setActionError(e);
    }
  };

  return (
    <>
      <BackLink />
      <PageHead
        title={t("session.title", { device: s.device_name ?? t("sessions.unknown_device") })}
        subtitle={`${formatDate(s.started_at, lang)} · ${s.customer_name ?? t("session.no_customer")}`}
        actions={
          rec && (
            <>
              <a className="btn btn-secondary" href={`/api/sessions/${s.id}/recording?download=1`}>
                <Icon name="download" />
                {t("session.download")}
              </a>
              <button className="btn btn-primary" onClick={() => setSharing(true)}>
                <Icon name="link" />
                {t("session.share")}
              </button>
            </>
          )
        }
      />
      {!!actionError && <Notice kind="error">{errorText(t, actionError)}</Notice>}
      <div className="detail-grid">
        <div className="card">
          <div className="card-head">
            <h2>{t("session.recording")}</h2>
            <RecordingPill s={s} />
          </div>
          {rec ? (
            <>
              <video className="player" controls preload="metadata" src={`/api/sessions/${s.id}/recording`} />
              <p className="hint">
                {t("session.recording_meta", { size: formatBytes(s.recording_bytes) })}
                {s.recording === "incomplete" && " " + t("session.recording_incomplete")}
              </p>
            </>
          ) : (
            <Empty icon="record" title={t("session.no_recording_" + s.recording)} />
          )}
          {rec && user.role === "admin" && (
            <div className="row">
              <button className="btn btn-sm btn-danger" onClick={() => setConfirmDelete(true)}>
                <Icon name="trash" />
                {t("session.delete_recording")}
              </button>
            </div>
          )}
        </div>
        <div className="card">
          <h2>{t("session.details")}</h2>
          <dl className="kv">
            <dt>{t("sessions.device")}</dt>
            <dd>
              {s.device_id ? <Link to={`/sessions?device=${s.device_id}`}>{s.device_name}</Link> : s.device_name ?? "-"}
            </dd>
            <dt>{t("sessions.customer")}</dt>
            <dd>{s.customer_name ?? "-"}</dd>
            <dt>{t("sessions.technician")}</dt>
            <dd>
              {s.user_name ?? "-"}
              {s.user_email && <span className="sub muted">{s.user_email}</span>}
            </dd>
            <dt>{t("sessions.kind")}</dt>
            <dd>{t("sessions.kind_" + s.kind)}</dd>
            <dt>{t("sessions.started")}</dt>
            <dd>{formatDate(s.started_at, lang)}</dd>
            <dt>{t("session.ended")}</dt>
            <dd>{s.ended_at ? formatDate(s.ended_at, lang) : <span className="pill ok">{t("sessions.live")}</span>}</dd>
            <dt>{t("sessions.duration")}</dt>
            <dd className="mono">{duration(s)}</dd>
            <dt>{t("sessions.connection")}</dt>
            <dd>{s.transport ? t("sessions.transport_" + s.transport, { defaultValue: s.transport }) : "-"}</dd>
            <dt>{t("session.viewer_ip")}</dt>
            <dd className="mono">{s.viewer_ip ?? "-"}</dd>
            {s.end_reason && (
              <>
                <dt>{t("session.end_reason")}</dt>
                <dd>{t("session.reason_" + s.end_reason, { defaultValue: s.end_reason })}</dd>
              </>
            )}
          </dl>
        </div>
      </div>
      <div className="card">
        <h2>{t("session.timeline")}</h2>
        <ol className="timeline">
          {data.events.map((e) => (
            <li key={e.id}>
              <time className="mono">{new Date(e.at).toLocaleTimeString(lang === "nl" ? "nl-NL" : "en-GB")}</time>
              <span>{eventText(t, e)}</span>
            </li>
          ))}
        </ol>
      </div>
      {data.shares.length > 0 && (
        <div className="card">
          <h2>{t("session.shares")}</h2>
          <div className="tbl-wrap">
            <table>
              <thead>
                <tr>
                  <th>{t("session.share_note")}</th>
                  <th>{t("session.share_by")}</th>
                  <th>{t("session.share_expires")}</th>
                  <th>{t("session.share_views")}</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {data.shares.map((sh) => {
                  const active = !sh.revoked_at && Date.parse(sh.expires_at) > Date.now();
                  return (
                    <tr key={sh.id}>
                      <td>{sh.note ?? <span className="muted">-</span>}</td>
                      <td>{sh.created_by ?? "-"}</td>
                      <td>
                        {formatDate(sh.expires_at, lang)}
                        {!active && <span className="sub">{sh.revoked_at ? t("session.share_revoked") : t("session.share_expired")}</span>}
                      </td>
                      <td className="mono">{sh.views}</td>
                      <td style={{ textAlign: "right" }}>
                        {active && (
                          <button className="btn btn-sm btn-secondary" onClick={() => revoke(sh)}>
                            {t("session.share_revoke")}
                          </button>
                        )}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        </div>
      )}
      {sharing && <ShareDialog session={s} defaultDays={data.share_days} onClose={() => (setSharing(false), reload())} />}
      {confirmDelete && (
        <Dialog title={t("session.delete_recording")} onClose={() => setConfirmDelete(false)}>
          <p>{t("session.delete_confirm")}</p>
          <div className="row">
            <button className="btn btn-danger" onClick={del}>
              {t("common.delete")}
            </button>
            <button className="btn btn-secondary" onClick={() => setConfirmDelete(false)}>
              {t("common.cancel")}
            </button>
          </div>
        </Dialog>
      )}
    </>
  );
}

function BackLink() {
  const { t } = useTranslation();
  return (
    <Link to="/sessions" className="back">
      <Icon name="back" />
      {t("session.back")}
    </Link>
  );
}

function ShareDialog({ session, defaultDays, onClose }: { session: RemoteSession; defaultDays: number; onClose: () => void }) {
  const { t } = useTranslation();
  const [days, setDays] = useState(defaultDays);
  const [note, setNote] = useState("");
  const [url, setUrl] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);
  const [err, setErr] = useState<unknown>(null);

  const create = async () => {
    try {
      const r = await api.post<{ url: string }>(`/sessions/${session.id}/shares`, { days, note });
      setUrl(r.url);
    } catch (e) {
      setErr(e);
    }
  };
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(url!);
      setCopied(true);
    } catch {
      /* the field is selectable as a fallback */
    }
  };

  return (
    <Dialog title={t("session.share")} onClose={onClose}>
      {!!err && <Notice kind="error">{errorText(t, err)}</Notice>}
      {url ? (
        <>
          <Notice kind="ok">{t("session.share_created")}</Notice>
          <div className="field">
            <label htmlFor="share-url">{t("session.share_link")}</label>
            <div className="row nowrap">
              <input id="share-url" className="input mono" readOnly value={url} onFocus={(e) => e.target.select()} />
              <button className="btn btn-secondary" onClick={copy}>
                <Icon name="copy" />
                {copied ? t("session.copied") : t("session.copy")}
              </button>
            </div>
            <span className="hint">{t("session.share_once")}</span>
          </div>
          <div className="row">
            <button className="btn btn-primary" onClick={onClose}>
              {t("common.close")}
            </button>
          </div>
        </>
      ) : (
        <>
          <p className="muted">{t("session.share_intro")}</p>
          <div className="grid2">
            <div className="field">
              <label htmlFor="share-days">{t("session.share_days")}</label>
              <input id="share-days" className="input" type="number" min={1} max={90} value={days} onChange={(e) => setDays(Number(e.target.value))} />
            </div>
            <div className="field">
              <label htmlFor="share-note">{t("session.share_note")}</label>
              <input id="share-note" className="input" maxLength={200} placeholder={t("session.share_note_hint")} value={note} onChange={(e) => setNote(e.target.value)} />
            </div>
          </div>
          <div className="row">
            <button className="btn btn-primary" onClick={create}>
              {t("session.share_create")}
            </button>
            <button className="btn btn-secondary" onClick={onClose}>
              {t("common.cancel")}
            </button>
          </div>
        </>
      )}
    </Dialog>
  );
}
