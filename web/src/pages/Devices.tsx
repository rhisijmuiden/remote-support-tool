import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { api } from "../api";
import { Icon } from "../components/Icon";
import { Dialog, Empty, Notice, PageHead, errorText, formatDate, useLoad } from "../components/ui";
import { useSession } from "../session";
import { formatCode } from "./Remote";

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
  const { user } = useSession();
  const nav = useNavigate();
  const { data, error, reload } = useLoad(() => api.get<Device[]>("/devices"));
  const [q, setQ] = useState("");
  const [adding, setAdding] = useState(false);
  const [busy, setBusy] = useState<number | null>(null);
  const [actionErr, setActionErr] = useState<unknown>(null);
  const [del, setDel] = useState<Device | null>(null);

  // Presence changes on its own; refresh every 10 seconds.
  useEffect(() => {
    const id = window.setInterval(reload, 10000);
    return () => window.clearInterval(id);
  });

  const list = (data ?? []).filter((d) => {
    const s = q.trim().toLowerCase().replace(/\s/g, "");
    return !s || d.hostname.toLowerCase().includes(s) || d.code.includes(s) || (d.customer ?? "").toLowerCase().includes(s);
  });

  const connect = async (d: Device) => {
    setBusy(d.id);
    setActionErr(null);
    try {
      const r = await api.post<{ session_id: string }>(`/devices/${d.id}/sessions`, {});
      nav(`/session/${r.session_id}`);
    } catch (e) {
      setActionErr(e);
      reload();
    } finally {
      setBusy(null);
    }
  };

  const remove = async () => {
    if (!del) return;
    try {
      await api.del(`/devices/${del.id}`);
      setDel(null);
      reload();
    } catch (e) {
      setActionErr(e);
      setDel(null);
    }
  };

  return (
    <>
      <PageHead
        title={t("devices.title")}
        subtitle={t("devices.subtitle")}
        actions={
          <>
            <input id="device-search" className="input" style={{ width: 240 }} placeholder={t("devices.search")} value={q} onChange={(e) => setQ(e.target.value)} />
            <button className="btn btn-primary" onClick={() => setAdding(true)}>
              <Icon name="plus" /> {t("devices.add")}
            </button>
          </>
        }
      />
      {!!error && <Notice kind="error">{errorText(t, error)}</Notice>}
      {!!actionErr && <Notice kind="error">{errorText(t, actionErr)}</Notice>}
      <div className="card">
        {data && data.length === 0 ? (
          <Empty icon="devices" title={t("devices.empty_title")}>
            {t("devices.empty")}
            <br />
            <button className="btn btn-primary" style={{ marginTop: 12 }} onClick={() => setAdding(true)}>
              <Icon name="plus" /> {t("devices.add")}
            </button>
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
                  <th aria-label={t("devices.actions")} />
                </tr>
              </thead>
              <tbody>
                {list.map((d) => (
                  <tr key={d.id}>
                    <td>
                      {d.hostname}
                      <span className="sub">{d.os_version}</span>
                    </td>
                    <td className="mono">{formatCode(d.code)}</td>
                    <td>{d.customer ?? "-"}</td>
                    <td>{d.last_user ?? "-"}</td>
                    <td className="mono">{d.agent_version ?? "-"}</td>
                    <td>{d.online ? "-" : formatDate(d.last_seen_at, i18n.language) || t("common.never")}</td>
                    <td>
                      <span className={"pill " + (d.online ? "ok" : "off")}>{t(d.online ? "status.online" : "status.offline")}</span>
                    </td>
                    <td className="actions">
                      <button className="btn btn-primary btn-sm" disabled={!d.online || busy === d.id} onClick={() => connect(d)}>
                        <Icon name="connect" /> {busy === d.id ? t("devices.connecting") : t("devices.connect")}
                      </button>
                      {user.role === "admin" && (
                        <button className="btn-icon" onClick={() => setDel(d)} title={t("common.delete")} aria-label={`${t("common.delete")} ${d.hostname}`}>
                          <Icon name="trash" />
                        </button>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
      {adding && <AddDevice onClose={() => setAdding(false)} />}
      {del && (
        <Dialog title={t("devices.delete_title")} onClose={() => setDel(null)}>
          <p>{t("devices.delete_body", { name: del.hostname })}</p>
          <div className="row" style={{ justifyContent: "flex-end", marginTop: 16 }}>
            <button className="btn btn-secondary" onClick={() => setDel(null)}>
              {t("common.cancel")}
            </button>
            <button className="btn btn-danger" onClick={remove}>
              {t("common.delete")}
            </button>
          </div>
        </Dialog>
      )}
    </>
  );
}

type Token = { id: number; label: string; customer: string | null; uses: number; max_uses: number | null; expires_at: string | null; created_by: string | null; token?: string };
type Customer = { id: number; name: string };

function AddDevice({ onClose }: { onClose: () => void }) {
  const { t, i18n } = useTranslation();
  const tokens = useLoad(() => api.get<Token[]>("/enrollment-tokens"));
  const customers = useLoad(() => api.get<Customer[]>("/customers"));
  const agent = useLoad(() => api.get<{ available: boolean; download_path: string }>("/agent"));
  const [label, setLabel] = useState("");
  const [customer, setCustomer] = useState("");
  const [days, setDays] = useState(7);
  const [single, setSingle] = useState(false);
  const [created, setCreated] = useState<Token | null>(null);
  const [err, setErr] = useState<unknown>(null);
  const [copied, setCopied] = useState("");

  const server = location.origin;
  const download = server + (agent.data?.download_path ?? "/api/agent/v1/download/windows");

  const create = async () => {
    setErr(null);
    try {
      const tk = await api.post<Token>("/enrollment-tokens", {
        label,
        customer_id: customer ? Number(customer) : null,
        valid_days: days,
        max_uses: single ? 1 : null,
      });
      setCreated(tk);
      tokens.reload();
    } catch (e) {
      setErr(e);
    }
  };

  const revoke = async (id: number) => {
    await api.del(`/enrollment-tokens/${id}`).catch(setErr);
    tokens.reload();
  };

  const ps = created
    ? `$f="$env:TEMP\\rhis-agent.exe"; Invoke-WebRequest "${download}" -OutFile $f; & $f install --server "${server}" --token "${created.token}"`
    : "";
  const cmd = created ? `rhis-agent.exe install --server ${server} --token ${created.token}` : "";

  const copy = (what: string, text: string) => {
    navigator.clipboard?.writeText(text).then(() => {
      setCopied(what);
      setTimeout(() => setCopied(""), 2000);
    });
  };

  return (
    <Dialog title={t("enroll.title")} onClose={onClose}>
      {!!err && <Notice kind="error">{errorText(t, err)}</Notice>}
      {location.protocol !== "https:" && <Notice kind="warn">{t("enroll.https_required")}</Notice>}
      {!created ? (
        <>
          <p className="muted">{t("enroll.intro")}</p>
          <div className="field">
            <label htmlFor="tk-label">{t("enroll.label")}</label>
            <input id="tk-label" className="input" value={label} placeholder={t("enroll.label_hint")} onChange={(e) => setLabel(e.target.value)} />
          </div>
          <div className="grid2">
            <div className="field">
              <label htmlFor="tk-customer">{t("enroll.customer")}</label>
              <select id="tk-customer" className="input" value={customer} onChange={(e) => setCustomer(e.target.value)}>
                <option value="">{t("enroll.no_customer")}</option>
                {(customers.data ?? []).map((c) => (
                  <option key={c.id} value={c.id}>
                    {c.name}
                  </option>
                ))}
              </select>
            </div>
            <div className="field">
              <label htmlFor="tk-days">{t("enroll.valid")}</label>
              <select id="tk-days" className="input" value={days} onChange={(e) => setDays(Number(e.target.value))}>
                {[1, 7, 30, 365, 0].map((d) => (
                  <option key={d} value={d}>
                    {d === 0 ? t("enroll.forever") : t("enroll.days", { count: d })}
                  </option>
                ))}
              </select>
            </div>
          </div>
          <label className="check">
            <input type="checkbox" checked={single} onChange={(e) => setSingle(e.target.checked)} /> {t("enroll.single_use")}
          </label>
          <div className="row" style={{ justifyContent: "flex-end", marginTop: 16 }}>
            <button className="btn btn-primary" onClick={create}>
              <Icon name="key" /> {t("enroll.create")}
            </button>
          </div>
        </>
      ) : (
        <>
          <Notice kind="ok">{t("enroll.created")}</Notice>
          <ol className="steps">
            <li>
              {t("enroll.step_download")}{" "}
              {agent.data?.available ? (
                <a href={download} download>
                  rhis-agent.exe
                </a>
              ) : (
                <span className="muted">{t("enroll.not_available")}</span>
              )}
            </li>
            <li>
              {t("enroll.step_run")}
              <pre className="code">{cmd}</pre>
              <button className="btn btn-secondary btn-sm" onClick={() => copy("cmd", cmd)}>
                <Icon name="copy" /> {copied === "cmd" ? t("enroll.copied") : t("enroll.copy")}
              </button>
            </li>
            <li>
              {t("enroll.step_ps")}
              <pre className="code">{ps}</pre>
              <button className="btn btn-secondary btn-sm" onClick={() => copy("ps", ps)}>
                <Icon name="copy" /> {copied === "ps" ? t("enroll.copied") : t("enroll.copy")}
              </button>
            </li>
          </ol>
          <p className="hint">{t("enroll.once")}</p>
          <div className="row" style={{ justifyContent: "flex-end", marginTop: 16 }}>
            <button className="btn btn-secondary" onClick={() => setCreated(null)}>
              {t("enroll.another")}
            </button>
            <button className="btn btn-primary" onClick={onClose}>
              {t("common.close")}
            </button>
          </div>
        </>
      )}
      {(tokens.data ?? []).length > 0 && (
        <div style={{ marginTop: 20 }}>
          <h3 className="sub-head">{t("enroll.active")}</h3>
          <div className="tbl-wrap">
            <table>
              <tbody>
                {(tokens.data ?? []).map((tk) => (
                  <tr key={tk.id}>
                    <td>
                      {tk.label || t("enroll.unnamed")}
                      <span className="sub">{tk.customer ?? t("enroll.no_customer")}</span>
                    </td>
                    <td className="muted">
                      {t("enroll.uses", { n: tk.uses })}
                      {tk.max_uses ? ` / ${tk.max_uses}` : ""}
                    </td>
                    <td className="muted">{tk.expires_at ? formatDate(tk.expires_at, i18n.language) : t("enroll.forever")}</td>
                    <td className="actions">
                      <button className="btn btn-secondary btn-sm" onClick={() => revoke(tk.id)}>
                        {t("enroll.revoke")}
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}
    </Dialog>
  );
}
