import { useState } from "react";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { api } from "../api";
import { Empty, Notice, PageHead, errorText, formatDate, useLoad } from "../components/ui";

type Device = {
  id: number;
  code: string;
  hostname: string;
  os_version: string | null;
  agent_version: string | null;
  last_user: string | null;
  last_seen_at: string | null;
  online: boolean;
  customer: string | null;
};

export function Devices() {
  const { t, i18n } = useTranslation();
  const { data, error } = useLoad(() => api.get<Device[]>("/devices"));
  const [q, setQ] = useState("");
  const list = (data ?? []).filter((d) => {
    const s = q.trim().toLowerCase().replace(/\s/g, "");
    return !s || d.hostname.toLowerCase().includes(s) || d.code.replace(/\s/g, "").includes(s) || (d.customer ?? "").toLowerCase().includes(s);
  });
  return (
    <>
      <PageHead
        title={t("devices.title")}
        subtitle={t("devices.subtitle")}
        actions={<input id="device-search" className="input" style={{ width: 240 }} placeholder={t("devices.search")} value={q} onChange={(e) => setQ(e.target.value)} />}
      />
      {!!error && <Notice kind="error">{errorText(t, error)}</Notice>}
      <div className="card">
        {data && data.length === 0 ? (
          <Empty icon="devices" title={t("devices.empty_title")}>
            {t("devices.empty")}
          </Empty>
        ) : data && list.length === 0 ? (
          <p className="muted">{t("devices.no_match")}</p>
        ) : (
          <div className="tbl-wrap">
            <table>
              <thead>
                <tr>
                  <th>{t("devices.device")}</th>
                  <th>{t("devices.id")}</th>
                  <th>{t("devices.customer")}</th>
                  <th>{t("devices.user")}</th>
                  <th>{t("devices.agent")}</th>
                  <th>{t("devices.last_seen")}</th>
                  <th>{t("devices.status")}</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {list.map((d) => (
                  <tr key={d.id}>
                    <td>
                      {d.hostname}
                      <span className="sub">{d.os_version}</span>
                    </td>
                    <td className="mono">{d.code}</td>
                    <td>{d.customer ?? "-"}</td>
                    <td>{d.last_user ?? "-"}</td>
                    <td className="mono">{d.agent_version ?? "-"}</td>
                    <td>{d.online ? "-" : formatDate(d.last_seen_at, i18n.language) || t("common.never")}</td>
                    <td>
                      <span className={"pill " + (d.online ? "ok" : "off")}>{t(d.online ? "status.online" : "status.offline")}</span>
                    </td>
                    <td>
                      <Link to={`/sessions?device=${d.id}`}>{t("devices.sessions")}</Link>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </>
  );
}
