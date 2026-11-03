import { useTranslation } from "react-i18next";
import { api } from "../api";
import { Empty, Notice, PageHead, errorText, useLoad } from "../components/ui";

type Customer = { id: number; name: string; devices: number; online: number };

export function Customers() {
  const { t } = useTranslation();
  const { data, error } = useLoad(() => api.get<Customer[]>("/customers"));
  return (
    <>
      <PageHead title={t("customers.title")} subtitle={t("customers.subtitle")} />
      {!!error && <Notice kind="error">{errorText(t, error)}</Notice>}
      <div className="card">
        {data && data.length === 0 ? (
          <Empty icon="customers" title={t("customers.empty_title")}>
            {t("customers.empty")}
          </Empty>
        ) : (
          <div className="tbl-wrap">
            <table>
              <thead>
                <tr>
                  <th>{t("customers.name")}</th>
                  <th>{t("customers.devices")}</th>
                  <th>{t("customers.online")}</th>
                </tr>
              </thead>
              <tbody>
                {(data ?? []).map((c) => (
                  <tr key={c.id}>
                    <td>{c.name}</td>
                    <td>{c.devices}</td>
                    <td>{c.online}</td>
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
