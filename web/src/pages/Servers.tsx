import { FormEvent, useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../api";
import { Icon } from "../components/Icon";
import { Dialog, Notice, PageHead, errorText, formatDate, useLoad } from "../components/ui";

export type ClusterNode = {
  id: string;
  name: string;
  region: string;
  roles: string[];
  version: string;
  address: string;
  relay_urls: string[];
  stats: { allocations?: number; load1?: number; cpus?: number; mem_mb?: number; uptime_s?: number; turns?: boolean };
  remote: boolean;
  draining: boolean;
  last_seen: string;
  online: boolean;
  self: boolean;
};
type NodesResponse = { nodes: ClusterNode[]; version: string; bus: "local" | "nats"; offline_after: number };
type JoinToken = { id: number; name: string; roles: string[]; created_at: string; expires_at: string };

type Action = { kind: "add" } | { kind: "remove"; node: ClusterNode } | { kind: "rotate" };

export function Servers() {
  const { t, i18n } = useTranslation();
  const nodes = useLoad(() => api.get<NodesResponse>("/cluster/nodes"));
  const tokens = useLoad(() => api.get<JoinToken[]>("/cluster/tokens"));
  const [action, setAction] = useState<Action | null>(null);
  const [err, setErr] = useState("");

  // The list follows heartbeats, so refresh it while the page is open.
  useEffect(() => {
    const id = window.setInterval(() => nodes.reload(), 10_000);
    return () => window.clearInterval(id);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const reload = () => (nodes.reload(), tokens.reload());
  const drain = async (n: ClusterNode) => {
    setErr("");
    try {
      await api.put(`/cluster/nodes/${n.id}/drain`, { draining: !n.draining });
      nodes.reload();
    } catch (e) {
      setErr(errorText(t, e));
    }
  };
  const revoke = async (tok: JoinToken) => {
    setErr("");
    try {
      await api.del(`/cluster/tokens/${tok.id}`);
      tokens.reload();
    } catch (e) {
      setErr(errorText(t, e));
    }
  };

  const list = nodes.data?.nodes ?? [];
  const online = list.filter((n) => n.online);
  const relays = list.filter((n) => n.roles.includes("relay"));
  const relaysOnline = relays.filter((n) => n.online && !n.draining);
  const allocations = relays.reduce((s, n) => s + (n.online ? n.stats.allocations ?? 0 : 0), 0);

  return (
    <>
      <PageHead
        title={t("servers.title")}
        subtitle={t("servers.subtitle")}
        actions={
          <>
            <button className="btn btn-secondary" onClick={() => setAction({ kind: "rotate" })}>
              {t("servers.rotate")}
            </button>
            <button className="btn btn-primary" onClick={() => setAction({ kind: "add" })}>
              {t("servers.add")}
            </button>
          </>
        }
      />
      {!!nodes.error && <Notice kind="error">{errorText(t, nodes.error)}</Notice>}
      {err && <Notice kind="error">{err}</Notice>}

      {nodes.data && (
        <div className="tiles">
          <div className={"tile" + (online.length < list.length ? " alert" : "")}>
            <span className="k">{t("servers.tile_online")}</span>
            <span className="v">
              {online.length}
              <small className="muted"> / {list.length}</small>
            </span>
            <span className="d">{online.length < list.length ? t("servers.some_offline", { n: list.length - online.length }) : t("servers.all_online")}</span>
          </div>
          <div className={"tile" + (relaysOnline.length === 0 ? " alert" : "")}>
            <span className="k">{t("servers.tile_relays")}</span>
            <span className="v">{relaysOnline.length}</span>
            <span className="d">{relaysOnline.length === 0 ? t("servers.no_relay") : t("servers.relays_hint")}</span>
          </div>
          <div className="tile">
            <span className="k">{t("servers.tile_allocations")}</span>
            <span className="v">{allocations}</span>
            <span className="d">{t("servers.allocations_hint")}</span>
          </div>
          <div className="tile">
            <span className="k">{t("servers.tile_bus")}</span>
            <span className="v" style={{ fontSize: "1.4rem" }}>
              {t("servers.bus_" + nodes.data.bus)}
            </span>
            <span className="d">{t("servers.bus_" + nodes.data.bus + "_hint")}</span>
          </div>
        </div>
      )}

      <div className="card">
        <div className="tbl-wrap">
          <table>
            <thead>
              <tr>
                <th>{t("servers.name")}</th>
                <th>{t("servers.roles")}</th>
                <th>{t("servers.status")}</th>
                <th>{t("servers.version")}</th>
                <th>{t("servers.load")}</th>
                <th>{t("servers.last_seen")}</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {list.map((n) => (
                <tr key={n.id}>
                  <td>
                    {n.name} {n.self && <span className="muted">({t("servers.this_server")})</span>}
                    <span className="sub">{[n.region, n.address, n.remote ? t("servers.joined") : ""].filter(Boolean).join(" · ")}</span>
                  </td>
                  <td>
                    <span className="row" style={{ gap: 6 }}>
                      {n.roles.map((r) => (
                        <span key={r} className="pill neutral">
                          {t("servers.role_" + r, { defaultValue: r })}
                        </span>
                      ))}
                    </span>
                  </td>
                  <td>
                    {!n.online ? (
                      <span className="pill off">{t("status.offline")}</span>
                    ) : n.draining ? (
                      <span className="pill warn">{t("servers.draining")}</span>
                    ) : (
                      <span className="pill ok">{t("status.online")}</span>
                    )}
                  </td>
                  <td>
                    <span className="mono" style={{ whiteSpace: "nowrap" }}>{n.version || "?"}</span>
                    {nodes.data && n.version && n.version !== nodes.data.version && <span className="sub warn-text">{t("servers.version_differs")}</span>}
                  </td>
                  <td>
                    {n.roles.includes("relay") && (
                      <span>
                        {t("servers.allocations", { n: n.stats.allocations ?? 0 })}
                        {n.stats.turns && <span className="sub">TURNS</span>}
                      </span>
                    )}
                    {n.stats.load1 != null && <span className="sub">{t("servers.cpu_load", { load: n.stats.load1.toFixed(2), cpus: n.stats.cpus ?? "?" })}</span>}
                  </td>
                  <td>{formatDate(n.last_seen, i18n.language)}</td>
                  <td style={{ textAlign: "right", whiteSpace: "nowrap" }}>
                    {n.roles.includes("relay") && (
                      <button className="btn btn-secondary btn-sm" onClick={() => drain(n)}>
                        {n.draining ? t("servers.resume") : t("servers.drain")}
                      </button>
                    )}
                    {!n.self && (
                      <button className="btn-icon" title={t("servers.remove")} aria-label={t("servers.remove")} onClick={() => setAction({ kind: "remove", node: n })}>
                        <Icon name="trash" />
                      </button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        <p className="hint">{t("servers.drain_help")}</p>
      </div>

      {(tokens.data ?? []).length > 0 && (
        <div className="card">
          <div className="card-head">
            <h2>{t("servers.pending")}</h2>
          </div>
          <div className="tbl-wrap">
            <table>
              <thead>
                <tr>
                  <th>{t("servers.name")}</th>
                  <th>{t("servers.roles")}</th>
                  <th>{t("servers.expires")}</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {(tokens.data ?? []).map((tok) => (
                  <tr key={tok.id}>
                    <td>{tok.name}</td>
                    <td>{tok.roles.map((r) => t("servers.role_" + r, { defaultValue: r })).join(", ")}</td>
                    <td>{formatDate(tok.expires_at, i18n.language)}</td>
                    <td style={{ textAlign: "right" }}>
                      <button className="btn btn-secondary btn-sm" onClick={() => revoke(tok)}>
                        {t("servers.revoke")}
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}

      <div className="card">
        <h2>{t("servers.scale_title")}</h2>
        <ol className="steps">
          <li>
            <strong>{t("servers.scale_1_title")}</strong> {t("servers.scale_1")}
          </li>
          <li>
            <strong>{t("servers.scale_2_title")}</strong> {t("servers.scale_2")}
          </li>
          <li>
            <strong>{t("servers.scale_3_title")}</strong> {t("servers.scale_3")}
          </li>
        </ol>
        <p className="hint">{t("servers.encryption")}</p>
      </div>

      {action?.kind === "add" && <AddDialog onClose={() => (setAction(null), reload())} />}
      {action?.kind === "remove" && (
        <ConfirmDialog
          title={t("servers.remove")}
          text={t("servers.confirm_remove", { name: action.node.name })}
          danger
          onClose={() => setAction(null)}
          onConfirm={async () => {
            await api.del(`/cluster/nodes/${action.node.id}`);
            nodes.reload();
          }}
        />
      )}
      {action?.kind === "rotate" && (
        <ConfirmDialog title={t("servers.rotate")} text={t("servers.confirm_rotate")} onClose={() => setAction(null)} onConfirm={() => api.post("/cluster/turn-secret/rotate")} />
      )}
    </>
  );
}

function ConfirmDialog({ title, text, danger, onClose, onConfirm }: { title: string; text: string; danger?: boolean; onClose: () => void; onConfirm: () => Promise<unknown> }) {
  const { t } = useTranslation();
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  const go = async () => {
    setBusy(true);
    try {
      await onConfirm();
      onClose();
    } catch (e) {
      setErr(errorText(t, e));
      setBusy(false);
    }
  };
  return (
    <Dialog title={title} onClose={onClose}>
      <div style={{ display: "grid", gap: 14 }}>
        {err && <Notice kind="error">{err}</Notice>}
        <p>{text}</p>
        <div className="row">
          <button className={"btn " + (danger ? "btn-danger" : "btn-primary")} disabled={busy} onClick={go}>
            {t("common.confirm")}
          </button>
          <button className="btn btn-secondary" onClick={onClose}>
            {t("common.cancel")}
          </button>
        </div>
      </div>
    </Dialog>
  );
}

function AddDialog({ onClose }: { onClose: () => void }) {
  const { t } = useTranslation();
  const [f, setF] = useState({ name: "", domain: "", ttl_hours: 24 });
  const [res, setRes] = useState<{ command: string } | null>(null);
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  const [copied, setCopied] = useState(false);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setErr("");
    try {
      setRes(await api.post<{ command: string }>("/cluster/tokens", { ...f, roles: ["relay"] }));
    } catch (e) {
      setErr(errorText(t, e));
    }
    setBusy(false);
  };
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(res!.command);
      setCopied(true);
    } catch {
      /* clipboard blocked: the command stays selectable */
    }
  };

  return (
    <Dialog title={t("servers.add")} onClose={onClose}>
      {!res ? (
        <form onSubmit={submit} style={{ display: "grid", gap: 14 }}>
          {err && <Notice kind="error">{err}</Notice>}
          <p className="muted">{t("servers.add_intro")}</p>
          <div className="field">
            <label htmlFor="s-name">{t("servers.name")}</label>
            <input id="s-name" className="input" required maxLength={100} placeholder="relay-ams-1" value={f.name} onChange={(e) => setF({ ...f, name: e.target.value })} />
          </div>
          <div className="field">
            <label htmlFor="s-domain">{t("servers.domain")}</label>
            <input id="s-domain" className="input" placeholder="relay1.example.com" value={f.domain} onChange={(e) => setF({ ...f, domain: e.target.value.trim() })} />
            <span className="hint">{t("servers.domain_hint")}</span>
          </div>
          <div className="field">
            <label htmlFor="s-ttl">{t("servers.valid_for")}</label>
            <select id="s-ttl" className="input" value={f.ttl_hours} onChange={(e) => setF({ ...f, ttl_hours: Number(e.target.value) })}>
              <option value={1}>{t("servers.hours", { count: 1 })}</option>
              <option value={24}>{t("servers.hours", { count: 24 })}</option>
              <option value={168}>{t("servers.days", { count: 7 })}</option>
            </select>
          </div>
          <div className="row">
            <button className="btn btn-primary" disabled={busy}>
              {t("servers.create_command")}
            </button>
            <button type="button" className="btn btn-secondary" onClick={onClose}>
              {t("common.cancel")}
            </button>
          </div>
        </form>
      ) : (
        <div style={{ display: "grid", gap: 14 }}>
          <p>{t("servers.run_command")}</p>
          <pre className="release-notes mono cmd">{res.command}</pre>
          <Notice kind="warn">{t("servers.token_once")}</Notice>
          <p className="hint">{t("servers.ports", { ports: "3478 udp/tcp, 49160-49359 udp" + (f.domain ? ", 443 tcp" : "") })}</p>
          <div className="row">
            <button className="btn btn-primary" onClick={copy}>
              {copied ? t("servers.copied") : t("servers.copy")}
            </button>
            <button className="btn btn-secondary" onClick={onClose}>
              {t("common.close")}
            </button>
          </div>
        </div>
      )}
    </Dialog>
  );
}
