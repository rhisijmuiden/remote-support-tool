// SPDX-License-Identifier: AGPL-3.0-only
import { FormEvent, useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { AgentOverview, AgentRelease, api, Customer, CustomerOverride, UpdatePolicy, upload } from "../api";
import { Icon } from "../components/Icon";
import { Dialog, Empty, Notice, PageHead, errorText, formatBytes, formatDate, useLoad } from "../components/ui";

type PolicyResp = { policy: UpdatePolicy; customers: CustomerOverride[] };
type SyncResult = { imported: AgentRelease[]; skipped: number; errors: string[] };

const statusPill: Record<string, string> = { offered: "neutral", downloaded: "warn", installed: "ok", failed: "off" };

export function AgentUpdates() {
  const { t, i18n } = useTranslation();
  const releases = useLoad(() => api.get<AgentRelease[]>("/agent/releases"));
  const pol = useLoad(() => api.get<PolicyResp>("/agent/policy"));
  const ov = useLoad(() => api.get<AgentOverview>("/agent/overview"));
  const [p, setP] = useState<UpdatePolicy | null>(null);
  const [dirty, setDirty] = useState(false);
  const [msg, setMsg] = useState<{ kind: "ok" | "error" | "warn"; text: string } | null>(null);
  const [busy, setBusy] = useState(false);
  const [uploading, setUploading] = useState(false);
  const [override, setOverride] = useState<Partial<CustomerOverride> | null>(null);

  useEffect(() => {
    if (pol.data && !dirty) setP(pol.data.policy);
  }, [pol.data]); // eslint-disable-line react-hooks/exhaustive-deps

  const act = async (fn: () => Promise<unknown>, ok?: string) => {
    setBusy(true);
    setMsg(null);
    try {
      await fn();
      if (ok) setMsg({ kind: "ok", text: ok });
    } catch (e) {
      setMsg({ kind: "error", text: errorText(t, e) });
    }
    setBusy(false);
  };

  const versions = (ch: "production" | "test") =>
    Array.from(new Set((releases.data ?? []).filter((r) => r.product === "agent" && (ch === "test" || r.channel === "production")).map((r) => r.version)));

  const save = (e: FormEvent) => {
    e.preventDefault();
    act(async () => {
      await api.put("/agent/policy", p);
      setDirty(false);
      pol.reload();
    }, t("common.saved"));
  };

  const sync = () =>
    act(async () => {
      const r = await api.post<SyncResult>("/agent/releases/sync");
      releases.reload();
      setMsg(
        r.errors.length
          ? { kind: "warn", text: t("agents.sync_errors", { n: r.imported.length }) + " " + r.errors.join("; ") }
          : { kind: "ok", text: r.imported.length ? t("agents.sync_imported", { n: r.imported.length }) : t("agents.sync_none") },
      );
    });

  const set = (x: Partial<UpdatePolicy>) => p && (setP({ ...p, ...x }), setDirty(true));

  return (
    <>
      <PageHead title={t("agents.title")} subtitle={t("agents.subtitle")} />
      {msg && <Notice kind={msg.kind}>{msg.text}</Notice>}

      <div className="tiles tiles-3">
        <div className="tile">
          <span className="k">{t("agents.devices_current")}</span>
          <span className="v">{ov.data ? ov.data.versions.reduce((n, v) => n + v.devices, 0) : "–"}</span>
          <span className="d">{t("agents.versions_n", { n: ov.data?.versions.length ?? 0 })}</span>
        </div>
        <div className="tile">
          <span className="k">{t("agents.newest")}</span>
          <span className="v mono-v">{versions("production")[0] ?? "–"}</span>
          <span className="d">{t("agents.channel_production")}</span>
        </div>
        <div className="tile">
          <span className="k">{t("agents.key")}</span>
          <span className="v mono-v small-v" title={ov.data?.public_key}>
            {ov.data?.fingerprint ?? "–"}
          </span>
          <span className="d">{t("agents.key_hint")}</span>
        </div>
      </div>

      {p && (
        <form className="card" onSubmit={save}>
          <h2>{t("agents.policy")}</h2>
          <label className="check">
            <input type="checkbox" checked={p.enabled} onChange={(e) => set({ enabled: e.target.checked })} />
            <span>
              <strong>{t("agents.enabled")}</strong>
              <span className="sub muted">{t("agents.enabled_hint")}</span>
            </span>
          </label>
          <div className="grid3">
            <div className="field">
              <label htmlFor="p-prod">{t("agents.production_version")}</label>
              <select id="p-prod" className="input" value={p.production_version} onChange={(e) => set({ production_version: e.target.value })}>
                <option value="">{t("agents.newest_option")}</option>
                {versions("production").map((v) => (
                  <option key={v}>{v}</option>
                ))}
              </select>
            </div>
            <div className="field">
              <label htmlFor="p-test">{t("agents.test_version")}</label>
              <select id="p-test" className="input" value={p.test_version} onChange={(e) => set({ test_version: e.target.value })}>
                <option value="">{t("agents.newest_option")}</option>
                {versions("test").map((v) => (
                  <option key={v}>{v}</option>
                ))}
              </select>
            </div>
            <div className="field">
              <label htmlFor="p-roll">
                {t("agents.rollout")}: <strong>{p.rollout_percent}%</strong>
              </label>
              <input id="p-roll" type="range" min={1} max={100} step={1} value={p.rollout_percent} onChange={(e) => set({ rollout_percent: Number(e.target.value) })} />
            </div>
          </div>
          <span className="hint">{t("agents.policy_hint")}</span>
          <div className="row">
            <button className="btn btn-primary" disabled={busy || !dirty}>
              {t("common.save")}
            </button>
          </div>
        </form>
      )}

      <div className="card">
        <div className="card-head">
          <h2>{t("agents.overrides")}</h2>
          <button className="btn btn-secondary btn-sm" onClick={() => setOverride({ mode: "off" })}>
            <Icon name="plus" /> {t("agents.override_add")}
          </button>
        </div>
        {(pol.data?.customers ?? []).length === 0 ? (
          <p className="muted">{t("agents.overrides_empty")}</p>
        ) : (
          <div className="tbl-wrap">
            <table>
              <thead>
                <tr>
                  <th>{t("agents.customer")}</th>
                  <th>{t("agents.rule")}</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {pol.data!.customers.map((o) => (
                  <tr key={o.customer_id}>
                    <td>{o.customer}</td>
                    <td>{o.mode === "off" ? <span className="pill off">{t("agents.mode_off")}</span> : <span className="pill warn">{t("agents.mode_pin", { v: o.version })}</span>}</td>
                    <td style={{ textAlign: "right" }}>
                      <button className="btn-icon" onClick={() => setOverride(o)} aria-label={t("common.edit")} title={t("common.edit")}>
                        <Icon name="edit" />
                      </button>
                      <button className="btn-icon" onClick={() => act(() => api.put(`/agent/policy/customers/${o.customer_id}`, { mode: "" }).then(pol.reload))} aria-label={t("common.delete")} title={t("common.delete")}>
                        <Icon name="trash" />
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>

      <div className="card">
        <div className="card-head">
          <h2>{t("agents.releases")}</h2>
          <div className="row">
            <button className="btn btn-secondary btn-sm" onClick={() => setUploading(true)}>
              <Icon name="upload" /> {t("agents.upload")}
            </button>
            <button className="btn btn-primary btn-sm" onClick={sync} disabled={busy}>
              <Icon name="updates" /> {t("agents.sync")}
            </button>
          </div>
        </div>
        {releases.data && releases.data.length === 0 ? (
          <Empty icon="builder" title={t("agents.releases_empty_title")}>
            {t("agents.releases_empty")}
          </Empty>
        ) : (
          <div className="tbl-wrap">
            <table>
              <thead>
                <tr>
                  <th>{t("agents.version")}</th>
                  <th>{t("agents.product")}</th>
                  <th>{t("agents.channel")}</th>
                  <th>{t("agents.source")}</th>
                  <th>{t("agents.released")}</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {(releases.data ?? []).map((r) => (
                  <tr key={r.id}>
                    <td>
                      <span className="mono">{r.version}</span>
                      <span className="sub mono" title={r.sha256}>
                        {r.sha256.slice(0, 16)}… · {formatBytes(r.size_bytes)}
                      </span>
                    </td>
                    <td>
                      {r.product === "quick" ? t("agents.product_quick") : t("agents.product_agent")} <span className="muted">{r.arch === "arm64" ? "ARM64" : "x64"}</span>
                    </td>
                    <td>{r.channel === "test" ? <span className="pill warn">{t("agents.channel_test")}</span> : <span className="pill ok">{t("agents.channel_production")}</span>}</td>
                    <td>{r.source === "github" ? "GitHub" : t("agents.source_upload")}</td>
                    <td>{formatDate(r.released_at, i18n.language)}</td>
                    <td style={{ textAlign: "right" }}>
                      <button className="btn-icon" onClick={() => act(() => api.del(`/agent/releases/${r.id}`).then(releases.reload))} aria-label={t("common.delete")} title={t("common.delete")}>
                        <Icon name="trash" />
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>

      <div className="grid-detail">
        <div className="card">
          <h2>{t("agents.distribution")}</h2>
          {(ov.data?.versions ?? []).length === 0 ? (
            <p className="muted">{t("agents.no_devices")}</p>
          ) : (
            <div className="bars">
              {ov.data!.versions.map((v) => {
                const total = ov.data!.versions.reduce((n, x) => n + x.devices, 0) || 1;
                return (
                  <div key={v.version} className="bar-row">
                    <span className="mono">{v.version || t("agents.unknown")}</span>
                    <span className="bar">
                      <i style={{ width: `${(v.devices / total) * 100}%` }} />
                    </span>
                    <span className="muted">{v.devices}</span>
                  </div>
                );
              })}
            </div>
          )}
        </div>
        <div className="card">
          <h2>{t("agents.activity")}</h2>
          {(ov.data?.events ?? []).length === 0 ? (
            <p className="muted">{t("agents.no_activity")}</p>
          ) : (
            <div className="tbl-wrap">
              <table>
                <tbody>
                  {ov.data!.events.slice(0, 12).map((e, i) => (
                    <tr key={i}>
                      <td>
                        {e.hostname ?? "–"}
                        <span className="sub">{formatDate(e.at, i18n.language)}</span>
                      </td>
                      <td className="mono">
                        {e.from_version ?? "?"} → {e.to_version}
                      </td>
                      <td>
                        <span className={"pill " + statusPill[e.status]} title={e.error ?? undefined}>
                          {t("agents.status_" + e.status)}
                        </span>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>
      </div>

      {uploading && <UploadDialog onClose={() => setUploading(false)} onDone={() => (setUploading(false), releases.reload(), setMsg({ kind: "ok", text: t("agents.uploaded") }))} />}
      {override && (
        <OverrideDialog
          o={override}
          versions={versions("test")}
          existing={(pol.data?.customers ?? []).map((c) => c.customer_id)}
          onClose={() => setOverride(null)}
          onDone={() => (setOverride(null), pol.reload())}
        />
      )}
    </>
  );
}

function UploadDialog({ onClose, onDone }: { onClose: () => void; onDone: () => void }) {
  const { t } = useTranslation();
  const [f, setF] = useState({ product: "agent", arch: "amd64", version: "", channel: "production" });
  const [file, setFile] = useState<File | null>(null);
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (!file) return;
    setBusy(true);
    setErr("");
    try {
      await upload("POST", "/agent/releases/upload?" + new URLSearchParams(f).toString(), file, "application/octet-stream");
      onDone();
    } catch (e) {
      setErr(errorText(t, e));
      setBusy(false);
    }
  };
  return (
    <Dialog title={t("agents.upload")} onClose={onClose}>
      <form onSubmit={submit} style={{ display: "grid", gap: 14 }}>
        {err && <Notice kind="error">{err}</Notice>}
        <div className="field">
          <label htmlFor="u-file">{t("agents.file")}</label>
          <input id="u-file" className="input" type="file" accept=".exe" required onChange={(e) => setFile(e.target.files?.[0] ?? null)} />
        </div>
        <div className="grid2">
          <div className="field">
            <label htmlFor="u-prod">{t("agents.product")}</label>
            <select id="u-prod" className="input" value={f.product} onChange={(e) => setF({ ...f, product: e.target.value })}>
              <option value="agent">{t("agents.product_agent")}</option>
              <option value="quick">{t("agents.product_quick")}</option>
            </select>
          </div>
          <div className="field">
            <label htmlFor="u-arch">{t("agents.arch")}</label>
            <select id="u-arch" className="input" value={f.arch} onChange={(e) => setF({ ...f, arch: e.target.value })}>
              <option value="amd64">x64</option>
              <option value="arm64">ARM64</option>
            </select>
          </div>
          <div className="field">
            <label htmlFor="u-ver">{t("agents.version")}</label>
            <input id="u-ver" className="input mono" required placeholder="v0.2.0" value={f.version} onChange={(e) => setF({ ...f, version: e.target.value })} />
          </div>
          <div className="field">
            <label htmlFor="u-ch">{t("agents.channel")}</label>
            <select id="u-ch" className="input" value={f.channel} onChange={(e) => setF({ ...f, channel: e.target.value })}>
              <option value="production">{t("agents.channel_production")}</option>
              <option value="test">{t("agents.channel_test")}</option>
            </select>
          </div>
        </div>
        <span className="hint">{t("agents.upload_hint")}</span>
        <div className="row">
          <button className="btn btn-primary" disabled={busy || !file}>
            {busy ? t("agents.uploading") : t("agents.upload")}
          </button>
          <button type="button" className="btn btn-secondary" onClick={onClose}>
            {t("common.cancel")}
          </button>
        </div>
      </form>
    </Dialog>
  );
}

function OverrideDialog({ o, versions, existing, onClose, onDone }: { o: Partial<CustomerOverride>; versions: string[]; existing: number[]; onClose: () => void; onDone: () => void }) {
  const { t } = useTranslation();
  const customers = useLoad(() => api.get<Customer[]>("/customers"));
  const [f, setF] = useState({ customer_id: o.customer_id ?? 0, mode: o.mode ?? "off", version: o.version ?? versions[0] ?? "" });
  const [err, setErr] = useState("");
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    try {
      await api.put(`/agent/policy/customers/${f.customer_id}`, { mode: f.mode, version: f.version });
      onDone();
    } catch (e) {
      setErr(errorText(t, e));
    }
  };
  const choices = (customers.data ?? []).filter((c) => c.id === o.customer_id || !existing.includes(c.id));
  return (
    <Dialog title={t("agents.override_add")} onClose={onClose}>
      <form onSubmit={submit} style={{ display: "grid", gap: 14 }}>
        {err && <Notice kind="error">{err}</Notice>}
        <div className="field">
          <label htmlFor="o-c">{t("agents.customer")}</label>
          <select id="o-c" className="input" required disabled={!!o.customer_id} value={f.customer_id || ""} onChange={(e) => setF({ ...f, customer_id: Number(e.target.value) })}>
            <option value="" disabled>
              {t("agents.choose_customer")}
            </option>
            {choices.map((c) => (
              <option key={c.id} value={c.id}>
                {c.name}
              </option>
            ))}
          </select>
        </div>
        <div className="seg" role="group">
          <button type="button" aria-pressed={f.mode === "off"} onClick={() => setF({ ...f, mode: "off" })}>
            {t("agents.mode_off_short")}
          </button>
          <button type="button" aria-pressed={f.mode === "pin"} onClick={() => setF({ ...f, mode: "pin" })}>
            {t("agents.mode_pin_short")}
          </button>
        </div>
        {f.mode === "pin" && (
          <div className="field">
            <label htmlFor="o-v">{t("agents.version")}</label>
            <select id="o-v" className="input" value={f.version} onChange={(e) => setF({ ...f, version: e.target.value })}>
              {versions.map((v) => (
                <option key={v}>{v}</option>
              ))}
            </select>
          </div>
        )}
        <span className="hint">{f.mode === "off" ? t("agents.mode_off_hint") : t("agents.mode_pin_hint")}</span>
        <div className="row">
          <button className="btn btn-primary" disabled={!f.customer_id}>
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
