import { useMemo, useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { api, duration, RemoteSession } from "../api";
import { Icon } from "../components/Icon";
import { Empty, Notice, PageHead, errorText, formatDate, useLoad } from "../components/ui";
import { useSession } from "../session";

type Page = { sessions: RemoteSession[]; total: number; recording_mode: string };
type Customer = { id: number; name: string };

const PAGE = 50;

export function RecordingPill({ s }: { s: RemoteSession }) {
  const { t } = useTranslation();
  if (s.recording === "none") return <span className="muted">-</span>;
  const cls = { recording: "rec", complete: "ok", incomplete: "warn", deleted: "neutral", none: "neutral" }[s.recording];
  return <span className={"pill " + cls}>{t("sessions.rec_" + s.recording)}</span>;
}

export function Sessions() {
  const { t, i18n } = useTranslation();
  const { user } = useSession();
  const nav = useNavigate();
  const [params, setParams] = useSearchParams();
  const [q, setQ] = useState(params.get("q") ?? "");
  const customers = useLoad(() => api.get<Customer[]>("/customers"));

  // The filters live in the URL so a filtered list can be bookmarked or linked from a device.
  const query = useMemo(() => {
    const p = new URLSearchParams();
    for (const k of ["q", "device", "customer", "from", "to", "recorded", "active", "offset"]) {
      const v = params.get(k);
      if (v) p.set(k, v);
    }
    p.set("limit", String(PAGE));
    return p;
  }, [params]);
  const { data, error } = useLoad(() => api.get<Page>("/sessions?" + query), [query.toString()]);

  const set = (k: string, v: string) => {
    const p = new URLSearchParams(params);
    if (v) p.set(k, v);
    else p.delete(k);
    p.delete("offset");
    setParams(p, { replace: true });
  };
  const offset = Number(params.get("offset") ?? 0);
  const setOffset = (n: number) => {
    const p = new URLSearchParams(params);
    if (n > 0) p.set("offset", String(n));
    else p.delete("offset");
    setParams(p, { replace: true });
  };
  const exportURL = "/api/sessions/export.csv?" + new URLSearchParams([...query].filter(([k]) => k !== "limit" && k !== "offset"));

  return (
    <>
      <PageHead
        title={t("sessions.title")}
        subtitle={user.role === "admin" ? t("sessions.subtitle_admin") : t("sessions.subtitle_own")}
        actions={
          <a className="btn btn-secondary" href={exportURL} download>
            <Icon name="download" />
            {t("sessions.export")}
          </a>
        }
      />
      {!!error && <Notice kind="error">{errorText(t, error)}</Notice>}
      {data?.recording_mode === "always" && <Notice>{t("sessions.mode_always")}</Notice>}
      <div className="card">
        <form
          className="filters"
          onSubmit={(e) => {
            e.preventDefault();
            set("q", q.trim());
          }}
        >
          <input className="input grow" placeholder={t("sessions.search")} value={q} onChange={(e) => setQ(e.target.value)} onBlur={() => set("q", q.trim())} aria-label={t("sessions.search")} />
          <select className="input" value={params.get("customer") ?? ""} onChange={(e) => set("customer", e.target.value)} aria-label={t("sessions.customer")}>
            <option value="">{t("sessions.all_customers")}</option>
            {(customers.data ?? []).map((c) => (
              <option key={c.id} value={c.id}>
                {c.name}
              </option>
            ))}
          </select>
          <label className="date">
            <span>{t("sessions.from")}</span>
            <input className="input" type="date" value={params.get("from") ?? ""} onChange={(e) => set("from", e.target.value)} />
          </label>
          <label className="date">
            <span>{t("sessions.to")}</span>
            <input className="input" type="date" value={params.get("to") ?? ""} onChange={(e) => set("to", e.target.value)} />
          </label>
          <label className="check">
            <input type="checkbox" checked={params.get("recorded") === "1"} onChange={(e) => set("recorded", e.target.checked ? "1" : "")} />
            {t("sessions.only_recorded")}
          </label>
          <label className="check">
            <input type="checkbox" checked={params.get("active") === "1"} onChange={(e) => set("active", e.target.checked ? "1" : "")} />
            {t("sessions.only_active")}
          </label>
          {params.get("device") && (
            <button type="button" className="btn btn-sm btn-secondary" onClick={() => set("device", "")}>
              {t("sessions.clear_device")}
            </button>
          )}
        </form>
        {data && data.total === 0 ? (
          <Empty icon="sessions" title={t("sessions.empty_title")}>
            {t("sessions.empty")}
          </Empty>
        ) : (
          <div className="tbl-wrap">
            <table className="clickable">
              <thead>
                <tr>
                  <th>{t("sessions.started")}</th>
                  <th>{t("sessions.device")}</th>
                  <th>{t("sessions.technician")}</th>
                  <th>{t("sessions.kind")}</th>
                  <th>{t("sessions.duration")}</th>
                  <th>{t("sessions.connection")}</th>
                  <th>{t("sessions.recording")}</th>
                </tr>
              </thead>
              <tbody>
                {(data?.sessions ?? []).map((s) => (
                  <tr key={s.id} onClick={() => nav(`/sessions/${s.id}`)}>
                    <td style={{ whiteSpace: "nowrap" }}>
                      <Link to={`/sessions/${s.id}`} onClick={(e) => e.stopPropagation()}>
                        {formatDate(s.started_at, i18n.language)}
                      </Link>
                    </td>
                    <td>
                      {s.device_name ?? <span className="muted">{t("sessions.unknown_device")}</span>}
                      <span className="sub">{s.customer_name ?? ""}</span>
                    </td>
                    <td>
                      {s.user_name ?? s.user_email ?? "-"}
                      <span className="sub mono">{s.viewer_ip ?? ""}</span>
                    </td>
                    <td>{t("sessions.kind_" + s.kind)}</td>
                    <td className="mono">
                      {s.ended_at ? duration(s) : <span className="pill ok">{t("sessions.live")}</span>}
                    </td>
                    <td>{s.transport ? t("sessions.transport_" + s.transport, { defaultValue: s.transport }) : "-"}</td>
                    <td>
                      <RecordingPill s={s} />
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        {data && data.total > PAGE && (
          <div className="row pager">
            <span className="muted">{t("sessions.range", { from: offset + 1, to: Math.min(offset + PAGE, data.total), total: data.total })}</span>
            <button className="btn btn-sm btn-secondary" disabled={offset === 0} onClick={() => setOffset(Math.max(0, offset - PAGE))}>
              {t("sessions.prev")}
            </button>
            <button
              className="btn btn-sm btn-secondary"
              disabled={offset + PAGE >= data.total}
              onClick={() => setOffset(offset + PAGE)}
            >
              {t("sessions.next")}
            </button>
          </div>
        )}
      </div>
    </>
  );
}
