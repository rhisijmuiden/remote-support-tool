import { useTranslation } from "react-i18next";
import { api } from "../api";
import { Empty, Notice, PageHead, errorText, formatDate, useLoad } from "../components/ui";

type Entry = { id: number; at: string; user_email: string | null; action: string; target: string | null; ip: string | null };

export function AuditLog() {
  const { t, i18n } = useTranslation();
  const { data, error } = useLoad(() => api.get<Entry[]>("/audit"));
  return (
    <>
      <PageHead title={t("audit.title")} subtitle={t("audit.subtitle")} />
      {!!error && <Notice kind="error">{errorText(t, error)}</Notice>}
      <div className="card">
        {data && data.length === 0 ? (
          <Empty icon="audit" title={t("audit.empty")} />
        ) : (
          <div className="tbl-wrap">
            <table>
              <thead>
                <tr>
                  <th>{t("audit.at")}</th>
                  <th>{t("audit.user")}</th>
                  <th>{t("audit.action")}</th>
                  <th>{t("audit.target")}</th>
                  <th>{t("audit.ip")}</th>
                </tr>
              </thead>
              <tbody>
                {(data ?? []).map((e) => (
                  <tr key={e.id}>
                    <td style={{ whiteSpace: "nowrap" }}>{formatDate(e.at, i18n.language)}</td>
                    <td>{e.user_email ?? <span className="muted">{t("audit.system")}</span>}</td>
                    <td className="mono">{e.action}</td>
                    <td>{e.target}</td>
                    <td className="mono">{e.ip}</td>
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
