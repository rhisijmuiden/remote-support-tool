// SPDX-License-Identifier: AGPL-3.0-only
import { FormEvent, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { AgentRelease, api, Build, BuildFile, Customer, upload } from "../api";
import { useSession } from "../session";
import { Icon } from "../components/Icon";
import { Dialog, Empty, Notice, PageHead, errorText, formatBytes, formatDate, useLoad } from "../components/ui";

const logoURL = (b: Build) => `/api/builds/${b.id}/logo?v=${encodeURIComponent(b.updated_at)}`;

/** ClientPreview shows roughly how the branded client window looks. */
function ClientPreview({ b, small }: { b: Pick<Build, "id" | "has_logo" | "updated_at" | "product_name" | "company_name" | "accent_color" | "mode">; small?: boolean }) {
  const { t } = useTranslation();
  const accent = b.accent_color || "#ff6b57";
  return (
    <div className={"client-preview" + (small ? " small" : "")} style={{ ["--accent" as string]: accent }}>
      <div className="cp-bar">
        <span className="cp-dots" aria-hidden="true">
          <i />
          <i />
          <i />
        </span>
        <span>{b.product_name}</span>
      </div>
      <div className="cp-body">
        <div className="cp-logo">{b.has_logo ? <img src={logoURL(b as Build)} alt="" /> : <span>{(b.company_name || b.product_name || "?").slice(0, 1)}</span>}</div>
        {!small && (
          <>
            <div className="cp-label">{b.mode === "quick" ? t("builds.preview_quick") : t("builds.preview_install")}</div>
            <div className="cp-code">{b.mode === "quick" ? "482 913 077" : "● " + t("builds.preview_connected")}</div>
            <div className="cp-company">{b.company_name}</div>
          </>
        )}
      </div>
    </div>
  );
}

export function Builds() {
  const { t } = useTranslation();
  const { user } = useSession();
  const admin = user.role === "admin";
  const { data, error, reload } = useLoad(() => api.get<Build[]>("/builds"));
  const releases = useLoad(() => api.get<AgentRelease[]>("/agent/releases"));
  const [creating, setCreating] = useState(false);
  const nav = useNavigate();
  return (
    <>
      <PageHead
        title={t("builds.title")}
        subtitle={t("builds.subtitle")}
        actions={
          admin && (
            <button className="btn btn-primary" onClick={() => setCreating(true)}>
              <Icon name="plus" /> {t("builds.add")}
            </button>
          )
        }
      />
      {!!error && <Notice kind="error">{errorText(t, error)}</Notice>}
      {releases.data && releases.data.length === 0 && (
        <Notice kind="warn">
          {t("builds.no_agent")} {admin && <Link to="/settings/agent-updates">{t("builds.no_agent_link")}</Link>}
        </Notice>
      )}
      {data && data.length === 0 ? (
        <div className="card">
          <Empty icon="builder" title={t("builds.empty_title")}>
            {t("builds.empty")}
          </Empty>
        </div>
      ) : (
        <div className="build-grid">
          {(data ?? []).map((b) => (
            <Link key={b.id} to={`/builds/${b.id}`} className={"build-card" + (b.revoked ? " revoked" : "")}>
              <ClientPreview b={b} small />
              <div className="bc-text">
                <strong>{b.name}</strong>
                <span className="muted">{b.customer ?? t("builds.no_customer")}</span>
                <span className="row" style={{ gap: 6 }}>
                  <span className="pill neutral">{b.mode === "quick" ? t("builds.mode_quick") : t("builds.mode_install")}</span>
                  {b.channel === "test" && <span className="pill warn">{t("builds.channel_test")}</span>}
                  {b.revoked && <span className="pill off">{t("builds.revoked")}</span>}
                </span>
              </div>
            </Link>
          ))}
        </div>
      )}
      {creating && <BuildDialog onClose={() => setCreating(false)} onDone={(b) => (setCreating(false), reload(), nav(`/builds/${b.id}`))} />}
    </>
  );
}

type Form = {
  name: string;
  customer_id: number | null;
  mode: "install" | "quick";
  product_name: string;
  company_name: string;
  accent_color: string;
  servers: string[];
  channel: "production" | "test";
};

function BuildDialog({ build, onClose, onDone }: { build?: Build; onClose: () => void; onDone: (b: Build) => void }) {
  const { t } = useTranslation();
  const { meta } = useSession();
  const customers = useLoad(() => api.get<Customer[]>("/customers"));
  const [f, setF] = useState<Form>(
    build
      ? { ...build, servers: [...build.servers] }
      : {
          name: "",
          customer_id: null,
          mode: "install",
          product_name: meta.product_name,
          company_name: meta.company_name,
          accent_color: "#FF6B57",
          servers: [location.protocol === "https:" ? location.origin : ""],
          channel: "production",
        },
  );
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  const set = (p: Partial<Form>) => setF({ ...f, ...p });
  const setServer = (i: number, v: string) => set({ servers: f.servers.map((s, j) => (j === i ? v : s)) });
  const move = (i: number, d: number) => {
    const s = [...f.servers];
    [s[i], s[i + d]] = [s[i + d], s[i]];
    set({ servers: s });
  };

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setErr("");
    try {
      if (build) {
        await api.put(`/builds/${build.id}`, f);
        onDone({ ...build, ...f });
      } else onDone(await api.post<Build>("/builds", f));
    } catch (e) {
      setErr(errorText(t, e));
      setBusy(false);
    }
  };

  return (
    <Dialog title={build ? t("builds.edit") : t("builds.add")} onClose={onClose}>
      <form onSubmit={submit} style={{ display: "grid", gap: 14 }}>
        {err && <Notice kind="error">{err}</Notice>}
        <div className="grid2">
          <div className="field">
            <label htmlFor="b-name">{t("builds.name")}</label>
            <input id="b-name" className="input" required value={f.name} placeholder={t("builds.name_placeholder")} onChange={(e) => set({ name: e.target.value })} />
          </div>
          <div className="field">
            <label htmlFor="b-cust">{t("builds.customer")}</label>
            <select id="b-cust" className="input" value={f.customer_id ?? ""} onChange={(e) => set({ customer_id: e.target.value ? Number(e.target.value) : null })}>
              <option value="">{t("builds.no_customer")}</option>
              {(customers.data ?? []).map((c) => (
                <option key={c.id} value={c.id}>
                  {c.name}
                </option>
              ))}
            </select>
          </div>
        </div>
        <div className="field">
          <span className="label">{t("builds.mode")}</span>
          <div className="seg" role="group" aria-label={t("builds.mode")}>
            <button type="button" aria-pressed={f.mode === "install"} onClick={() => set({ mode: "install" })}>
              {t("builds.mode_install")}
            </button>
            <button type="button" aria-pressed={f.mode === "quick"} onClick={() => set({ mode: "quick" })}>
              {t("builds.mode_quick")}
            </button>
          </div>
          <span className="hint">{f.mode === "quick" ? t("builds.mode_quick_hint") : t("builds.mode_install_hint")}</span>
        </div>
        <div className="grid3">
          <div className="field">
            <label htmlFor="b-prod">{t("builds.product_name")}</label>
            <input id="b-prod" className="input" required maxLength={60} value={f.product_name} onChange={(e) => set({ product_name: e.target.value })} />
          </div>
          <div className="field">
            <label htmlFor="b-comp">{t("builds.company_name")}</label>
            <input id="b-comp" className="input" maxLength={80} value={f.company_name} onChange={(e) => set({ company_name: e.target.value })} />
          </div>
          <div className="field">
            <label htmlFor="b-color">{t("builds.accent")}</label>
            <span className="row" style={{ flexWrap: "nowrap", gap: 6 }}>
              <input type="color" aria-label={t("builds.accent")} className="swatch" value={f.accent_color || "#ff6b57"} onChange={(e) => set({ accent_color: e.target.value.toUpperCase() })} />
              <input id="b-color" className="input mono" value={f.accent_color} pattern="#[0-9a-fA-F]{6}" onChange={(e) => set({ accent_color: e.target.value })} />
            </span>
          </div>
        </div>
        <div className="field">
          <span className="label">{t("builds.servers")}</span>
          <div className="server-list">
            {f.servers.map((s, i) => (
              <div key={i} className="server-row">
                <span className="num">{i + 1}</span>
                <input className="input mono" aria-label={`${t("builds.server")} ${i + 1}`} placeholder="https://remote.example.com" value={s} onChange={(e) => setServer(i, e.target.value)} />
                <button type="button" className="btn-icon" disabled={i === 0} onClick={() => move(i, -1)} aria-label={t("builds.move_up")} title={t("builds.move_up")}>
                  <Icon name="up" />
                </button>
                <button type="button" className="btn-icon" disabled={i === f.servers.length - 1} onClick={() => move(i, 1)} aria-label={t("builds.move_down")} title={t("builds.move_down")}>
                  <Icon name="down" />
                </button>
                <button type="button" className="btn-icon" disabled={f.servers.length === 1} onClick={() => set({ servers: f.servers.filter((_, j) => j !== i) })} aria-label={t("common.delete")} title={t("common.delete")}>
                  <Icon name="trash" />
                </button>
              </div>
            ))}
          </div>
          <div>
            <button type="button" className="btn btn-secondary btn-sm" onClick={() => set({ servers: [...f.servers, ""] })} disabled={f.servers.length >= 16}>
              <Icon name="plus" /> {t("builds.add_server")}
            </button>
          </div>
          <span className="hint">{t("builds.servers_hint")}</span>
        </div>
        <div className="field" style={{ maxWidth: 260 }}>
          <label htmlFor="b-ch">{t("builds.channel")}</label>
          <select id="b-ch" className="input" value={f.channel} onChange={(e) => set({ channel: e.target.value as Form["channel"] })}>
            <option value="production">{t("builds.channel_production")}</option>
            <option value="test">{t("builds.channel_test")}</option>
          </select>
          <span className="hint">{t("builds.channel_hint")}</span>
        </div>
        <div className="row">
          <button className="btn btn-primary" disabled={busy}>
            {t("common.save")}
          </button>
          <button type="button" className="btn btn-secondary" onClick={onClose}>
            {t("common.cancel")}
          </button>
        </div>
      </form>
    </Dialog>
  );
}

type Confirm = "delete" | "revoke" | "rotate";

export function BuildDetail() {
  const { t, i18n } = useTranslation();
  const { user } = useSession();
  const admin = user.role === "admin";
  const id = Number(useParams().id);
  const nav = useNavigate();
  const { data: b, error, reload } = useLoad(() => api.get<Build>(`/builds/${id}`), [id]);
  const [editing, setEditing] = useState(false);
  const [confirm, setConfirm] = useState<Confirm | null>(null);
  const [linkFor, setLinkFor] = useState<BuildFile | null>(null);
  const [arch, setArch] = useState<"amd64" | "arm64">("amd64");
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<{ kind: "ok" | "error"; text: string } | null>(null);

  const act = async (fn: () => Promise<unknown>, ok?: string) => {
    setBusy(true);
    setMsg(null);
    try {
      await fn();
      if (ok) setMsg({ kind: "ok", text: ok });
      reload();
    } catch (e) {
      setMsg({ kind: "error", text: errorText(t, e) });
    }
    setBusy(false);
  };

  if (error) return <Notice kind="error">{errorText(t, error)}</Notice>;
  if (!b) return <PageHead title={t("builds.title")} />;

  const pickLogo = () => {
    const inp = document.createElement("input");
    inp.type = "file";
    inp.accept = "image/png";
    inp.onchange = () => inp.files?.[0] && act(() => upload("PUT", `/builds/${b.id}/logo`, inp.files![0], "image/png"), t("builds.logo_saved"));
    inp.click();
  };

  const copy = (s: string) => navigator.clipboard?.writeText(s);

  return (
    <>
      <Link to="/builds" className="back-link">
        <Icon name="back" /> {t("builds.title")}
      </Link>
      <PageHead
        title={b.name}
        subtitle={(b.customer ?? t("builds.no_customer")) + " · " + (b.mode === "quick" ? t("builds.mode_quick") : t("builds.mode_install"))}
        actions={
          admin && (
            <>
              <button className="btn btn-secondary btn-sm" onClick={() => setEditing(true)}>
                <Icon name="edit" /> {t("common.edit")}
              </button>
              {b.revoked ? (
                <button className="btn btn-secondary btn-sm" onClick={() => act(() => api.post(`/builds/${b.id}/restore`))} disabled={busy}>
                  {t("builds.restore")}
                </button>
              ) : (
                <button className="btn btn-secondary btn-sm" onClick={() => setConfirm("revoke")}>
                  <Icon name="ban" /> {t("builds.revoke")}
                </button>
              )}
              <button className="btn btn-danger btn-sm" onClick={() => setConfirm("delete")}>
                <Icon name="trash" /> {t("common.delete")}
              </button>
            </>
          )
        }
      />
      {msg && <Notice kind={msg.kind}>{msg.text}</Notice>}
      {b.revoked && <Notice kind="warn">{t("builds.revoked_notice")}</Notice>}
      <div className="grid-detail">
        <div className="card">
          <h2>{t("builds.branding")}</h2>
          <ClientPreview b={b} />
          {admin && (
            <div className="row">
              <button className="btn btn-secondary btn-sm" onClick={pickLogo} disabled={busy}>
                <Icon name="upload" /> {b.has_logo ? t("builds.logo_change") : t("builds.logo_upload")}
              </button>
              {b.has_logo && (
                <button className="btn btn-secondary btn-sm" onClick={() => act(() => api.del(`/builds/${b.id}/logo`))} disabled={busy}>
                  {t("builds.logo_remove")}
                </button>
              )}
            </div>
          )}
          <span className="hint">{t("builds.logo_hint")}</span>
        </div>
        <div className="card">
          <h2>{t("builds.connection")}</h2>
          <dl className="kv">
            <dt>{t("builds.servers")}</dt>
            <dd>
              <ol className="plain-ol">
                {b.servers.map((s) => (
                  <li key={s} className="mono">
                    {s}
                  </li>
                ))}
              </ol>
            </dd>
            <dt>{t("builds.channel")}</dt>
            <dd>{b.channel === "test" ? t("builds.channel_test") : t("builds.channel_production")}</dd>
            <dt>{t("builds.encryption")}</dt>
            <dd>{t("builds.encryption_value")}</dd>
            <dt>{t("builds.enrollment")}</dt>
            <dd>
              {b.revoked ? <span className="pill off">{t("builds.revoked")}</span> : <span className="pill ok">{t("builds.enroll_active")}</span>}
              {admin && !b.revoked && (
                <button className="btn-link" onClick={() => setConfirm("rotate")}>
                  {t("builds.rotate")}
                </button>
              )}
            </dd>
          </dl>
        </div>
      </div>

      <div className="card">
        <div className="card-head">
          <h2>{t("builds.installers")}</h2>
          <div className="row">
            <select className="input" style={{ width: "auto" }} aria-label={t("builds.arch")} value={arch} onChange={(e) => setArch(e.target.value as "amd64")}>
              <option value="amd64">Windows x64</option>
              <option value="arm64">Windows ARM64</option>
            </select>
            <button className="btn btn-primary btn-sm" disabled={busy || b.revoked} onClick={() => act(() => api.post(`/builds/${b.id}/files`, { arch }), t("builds.generated"))}>
              <Icon name="builder" /> {busy ? t("builds.generating") : t("builds.generate")}
            </button>
          </div>
        </div>
        {(b.files ?? []).length === 0 ? (
          <Empty icon="download" title={t("builds.no_files_title")}>
            {t("builds.no_files")}
          </Empty>
        ) : (
          <div className="tbl-wrap">
            <table>
              <thead>
                <tr>
                  <th>{t("builds.file")}</th>
                  <th>{t("builds.agent_version")}</th>
                  <th>{t("builds.signature")}</th>
                  <th>{t("builds.links")}</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {(b.files ?? []).map((f) => (
                  <tr key={f.id}>
                    <td>
                      <span className="mono">{f.file_name}</span>
                      <span className="sub">
                        {f.arch === "arm64" ? "ARM64" : "x64"} · {formatBytes(f.size_bytes)} · {formatDate(f.created_at, i18n.language)}
                      </span>
                      <span className="sub mono" title="SHA-256">
                        {f.sha256.slice(0, 16)}…
                      </span>
                    </td>
                    <td className="mono">{f.agent_version}</td>
                    <td>{f.signed ? <span className="pill ok">{t("builds.signed")}</span> : <span className="pill warn">{t("builds.unsigned")}</span>}</td>
                    <td>
                      {f.links.filter((l) => l.active).length === 0 ? (
                        <span className="muted">{t("builds.no_links")}</span>
                      ) : (
                        f.links
                          .filter((l) => l.active)
                          .map((l) => (
                            <span key={l.id} className="sub">
                              {t("builds.link_until", { date: formatDate(l.expires_at, i18n.language) })} · {l.downloads}
                              {l.max_downloads ? `/${l.max_downloads}` : ""} {t("builds.downloads")}
                              <button className="btn-link" onClick={() => act(() => api.del(`/builds/${b.id}/links/${l.id}`))}>
                                {t("builds.link_revoke")}
                              </button>
                            </span>
                          ))
                      )}
                    </td>
                    <td style={{ textAlign: "right", whiteSpace: "nowrap" }}>
                      <a className="btn-icon" href={`/api/builds/${b.id}/files/${f.id}/download`} title={t("builds.download")} aria-label={t("builds.download")}>
                        <Icon name="download" />
                      </a>
                      <button className="btn-icon" disabled={b.revoked} onClick={() => setLinkFor(f)} title={t("builds.share")} aria-label={t("builds.share")}>
                        <Icon name="link" />
                      </button>
                      {admin && (
                        <button className="btn-icon" onClick={() => act(() => api.del(`/builds/${b.id}/files/${f.id}`))} title={t("common.delete")} aria-label={t("common.delete")}>
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
        {!(b.files ?? []).some((f) => f.signed) && (b.files ?? []).length > 0 && <span className="hint">{t("builds.unsigned_hint")}</span>}
      </div>

      {editing && <BuildDialog build={b} onClose={() => setEditing(false)} onDone={() => (setEditing(false), reload())} />}
      {linkFor && <LinkDialog build={b} file={linkFor} onClose={() => (setLinkFor(null), reload())} copy={copy} />}
      {confirm && (
        <Dialog title={t("builds.confirm_" + confirm + "_title")} onClose={() => setConfirm(null)}>
          <p>{t("builds.confirm_" + confirm, { name: b.name })}</p>
          <div className="row">
            <button
              className={"btn " + (confirm === "delete" ? "btn-danger" : "btn-primary")}
              disabled={busy}
              onClick={() => {
                const c = confirm;
                setConfirm(null);
                if (c === "delete") api.del(`/builds/${b.id}`).then(() => nav("/builds"), (e) => setMsg({ kind: "error", text: errorText(t, e) }));
                if (c === "revoke") act(() => api.post(`/builds/${b.id}/revoke`));
                if (c === "rotate") act(() => api.post(`/builds/${b.id}/rotate-token`), t("builds.rotated"));
              }}
            >
              {t("builds.confirm_" + confirm + "_button")}
            </button>
            <button className="btn btn-secondary" onClick={() => setConfirm(null)}>
              {t("common.cancel")}
            </button>
          </div>
        </Dialog>
      )}
    </>
  );
}

function LinkDialog({ build, file, onClose, copy }: { build: Build; file: BuildFile; onClose: () => void; copy: (s: string) => void }) {
  const { t } = useTranslation();
  const [days, setDays] = useState(7);
  const [max, setMax] = useState("");
  const [url, setURL] = useState("");
  const [err, setErr] = useState("");
  const [copied, setCopied] = useState(false);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setErr("");
    try {
      const r = await api.post<{ url: string }>(`/builds/${build.id}/files/${file.id}/links`, { days, max_downloads: max ? Number(max) : null });
      setURL(r.url);
    } catch (e) {
      setErr(errorText(t, e));
    }
  };
  return (
    <Dialog title={t("builds.share")} onClose={onClose}>
      {err && <Notice kind="error">{err}</Notice>}
      {url ? (
        <div style={{ display: "grid", gap: 12 }}>
          <Notice kind="ok">{t("builds.link_ready")}</Notice>
          <div className="row" style={{ flexWrap: "nowrap" }}>
            <input className="input mono" readOnly value={url} onFocus={(e) => e.target.select()} aria-label={t("builds.link")} />
            <button className="btn btn-primary" onClick={() => (copy(url), setCopied(true))}>
              <Icon name="copy" /> {copied ? t("builds.copied") : t("builds.copy")}
            </button>
          </div>
          <span className="hint">{t("builds.link_once")}</span>
        </div>
      ) : (
        <form onSubmit={submit} style={{ display: "grid", gap: 14 }}>
          <p className="muted">{t("builds.share_hint", { file: file.file_name })}</p>
          <div className="grid2">
            <div className="field">
              <label htmlFor="l-days">{t("builds.link_days")}</label>
              <select id="l-days" className="input" value={days} onChange={(e) => setDays(Number(e.target.value))}>
                {[1, 3, 7, 14, 30, 90].map((d) => (
                  <option key={d} value={d}>
                    {t("builds.days", { count: d })}
                  </option>
                ))}
              </select>
            </div>
            <div className="field">
              <label htmlFor="l-max">{t("builds.link_max")}</label>
              <input id="l-max" className="input" type="number" min={1} max={10000} placeholder={t("builds.unlimited")} value={max} onChange={(e) => setMax(e.target.value)} />
            </div>
          </div>
          <div className="row">
            <button className="btn btn-primary">{t("builds.create_link")}</button>
            <button type="button" className="btn btn-secondary" onClick={onClose}>
              {t("common.cancel")}
            </button>
          </div>
        </form>
      )}
    </Dialog>
  );
}
