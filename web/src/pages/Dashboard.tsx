import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { api, BackupRun } from "../api";
import { useSession } from "../session";
import { Empty, Notice, PageHead, errorText, formatBytes, formatDate, useLoad } from "../components/ui";

type Dash = {
  clients: number;
  online: number;
  offline: number;
  offline_over_week: number;
  active_sessions: number;
  customers: number;
  last_backup: BackupRun | null;
};

export function Dashboard() {
  const { t, i18n } = useTranslation();
  const { user } = useSession();
  const { data, error } = useLoad(() => api.get<Dash>("/dashboard"));
  const pct = data && data.clients > 0 ? Math.round((data.online / data.clients) * 1000) / 10 : 0;
  return (
    <>
      <PageHead title={t("dashboard.title")} subtitle={t("dashboard.subtitle")} />
      {!!error && <Notice kind="error">{errorText(t, error)}</Notice>}
      {data && (
        <>
          <div className="tiles">
            <div className="tile">
              <span className="k">{t("dashboard.clients")}</span>
              <span className="v">{data.clients}</span>
              <span className="d">{t("dashboard.clients_d", { count: data.customers })}</span>
            </div>
            <div className="tile">
              <span className="k">{t("dashboard.online")}</span>
              <span className="v">{data.online}</span>
              <span className="d">{t("dashboard.online_d", { pct: pct.toLocaleString(i18n.language) })}</span>
            </div>
            <div className={"tile" + (data.offline > 0 ? " alert" : "")}>
              <span className="k">{t("dashboard.offline")}</span>
              <span className="v">{data.offline}</span>
              <span className="d">{t("dashboard.offline_d", { count: data.offline_over_week })}</span>
            </div>
            <div className="tile">
              <span className="k">{t("dashboard.sessions")}</span>
              <span className="v">{data.active_sessions}</span>
              <span className="d">{t("dashboard.sessions_d")}</span>
            </div>
          </div>
          {data.clients === 0 && (
            <div className="card">
              <Empty icon="devices" title={t("dashboard.no_clients_title")}>
                {t("dashboard.no_clients")}
              </Empty>
            </div>
          )}
          <div className="card">
            <div className="card-head">
              <h2>{t("dashboard.last_backup")}</h2>
              {user.role === "admin" && (
                <Link className="btn btn-secondary btn-sm" to="/settings/backup">
                  {t("dashboard.setup_backup")}
                </Link>
              )}
            </div>
            {data.last_backup ? (
              <div className="row">
                <span className={"pill " + (data.last_backup.status === "ok" ? "ok" : data.last_backup.status === "failed" ? "off" : "warn")}>
                  {t("status." + data.last_backup.status)}
                </span>
                <span>{t("backup." + data.last_backup.kind)}</span>
                <span className="muted">{formatDate(data.last_backup.started_at, i18n.language)}</span>
                <span className="muted">{formatBytes(data.last_backup.size_bytes)}</span>
              </div>
            ) : (
              <p className="muted">{t("dashboard.no_backup")}</p>
            )}
          </div>
        </>
      )}
    </>
  );
}
