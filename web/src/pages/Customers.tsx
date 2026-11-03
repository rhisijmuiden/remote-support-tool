import { FormEvent, useState } from "react";
import { useTranslation } from "react-i18next";
import { api, Customer } from "../api";
import { useSession } from "../session";
import { Icon } from "../components/Icon";
import { Dialog, Empty, Notice, PageHead, errorText, useLoad } from "../components/ui";

type Edit = { mode: "new" } | { mode: "edit"; c: Customer } | { mode: "delete"; c: Customer };

export function Customers() {
  const { t } = useTranslation();
  const { user } = useSession();
  const admin = user.role === "admin";
  const { data, error, reload } = useLoad(() => api.get<Customer[]>("/customers"));
  const [edit, setEdit] = useState<Edit | null>(null);
  return (
    <>
      <PageHead
        title={t("customers.title")}
        subtitle={t("customers.subtitle")}
        actions={
          admin && (
            <button className="btn btn-primary" onClick={() => setEdit({ mode: "new" })}>
              <Icon name="plus" /> {t("customers.add")}
            </button>
          )
        }
      />
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
                  {admin && <th />}
                </tr>
              </thead>
              <tbody>
                {(data ?? []).map((c) => (
                  <tr key={c.id}>
                    <td>{c.name}</td>
                    <td>{c.devices}</td>
                    <td>{c.online}</td>
                    {admin && (
                      <td style={{ textAlign: "right", whiteSpace: "nowrap" }}>
                        <button className="btn-icon" title={t("common.edit")} aria-label={t("common.edit")} onClick={() => setEdit({ mode: "edit", c })}>
                          <Icon name="edit" />
                        </button>
                        <button className="btn-icon" title={t("common.delete")} aria-label={t("common.delete")} onClick={() => setEdit({ mode: "delete", c })}>
                          <Icon name="trash" />
                        </button>
                      </td>
                    )}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
      {edit && <CustomerDialog edit={edit} onClose={() => setEdit(null)} onDone={() => (setEdit(null), reload())} />}
    </>
  );
}

function CustomerDialog({ edit, onClose, onDone }: { edit: Edit; onClose: () => void; onDone: () => void }) {
  const { t } = useTranslation();
  const [name, setName] = useState(edit.mode === "new" ? "" : edit.c.name);
  const [err, setErr] = useState("");
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    try {
      if (edit.mode === "new") await api.post("/customers", { name });
      if (edit.mode === "edit") await api.put(`/customers/${edit.c.id}`, { name });
      if (edit.mode === "delete") await api.del(`/customers/${edit.c.id}`);
      onDone();
    } catch (e) {
      setErr(errorText(t, e));
    }
  };
  return (
    <Dialog title={edit.mode === "new" ? t("customers.add") : edit.mode === "edit" ? t("common.edit") : t("common.delete")} onClose={onClose}>
      <form onSubmit={submit} style={{ display: "grid", gap: 14 }}>
        {err && <Notice kind="error">{err}</Notice>}
        {edit.mode === "delete" ? (
          <p>{t("customers.confirm_delete", { name: edit.c.name })}</p>
        ) : (
          <div className="field">
            <label htmlFor="c-name">{t("customers.name")}</label>
            <input id="c-name" className="input" required maxLength={120} value={name} onChange={(e) => setName(e.target.value)} />
          </div>
        )}
        <div className="row">
          <button className={"btn " + (edit.mode === "delete" ? "btn-danger" : "btn-primary")}>{edit.mode === "delete" ? t("common.delete") : t("common.save")}</button>
          <button type="button" className="btn btn-secondary" onClick={onClose}>
            {t("common.cancel")}
          </button>
        </div>
      </form>
    </Dialog>
  );
}
