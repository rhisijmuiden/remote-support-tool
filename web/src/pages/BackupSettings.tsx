import { FormEvent, useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { api, BackupRun } from "../api";
import { useSession } from "../session";
import { Icon } from "../components/Icon";
import { Dialog, Notice, PageHead, errorText, formatBytes, formatDate, useLoad } from "../components/ui";

type Target = {
  type: "scp" | "ftp" | "ftps-explicit" | "ftps-implicit";
  host: string;
  port: number;
  username: string;
  password?: string;
  private_key?: string;
  host_key: string;
  skip_tls_verify: boolean;
  dir: string;
};
type Backup = {
  target: Target;
  full_schedule: string;
  incremental_schedule: string;
  keep_full: number;
  passphrase?: string;
  has_password?: boolean;
  has_private_key?: boolean;
  has_passphrase?: boolean;
};
type Remote = { name: string; size: number; mod_time: string };

const defaultPorts = { scp: 22, ftp: 21, "ftps-explicit": 21, "ftps-implicit": 990 };

export function BackupSettings() {
  const { t, i18n } = useTranslation();
  const { setUser } = useSession();
  const [f, setF] = useState<Backup | null>(null);
  const [dirty, setDirty] = useState(false);
  const [msg, setMsg] = useState<{ kind: "ok" | "error" | "info"; text: string } | null>(null);
  const [busy, setBusy] = useState(false);
  const runs = useLoad(() => api.get<BackupRun[]>("/backup/runs"));
  const [remote, setRemote] = useState<Remote[] | null>(null);
  const [restore, setRestore] = useState<string | null>(null);

  useEffect(() => {
    api.get<Backup>("/settings/backup").then(setF, (e) => setMsg({ kind: "error", text: errorText(t, e) }));
  }, [t]);

  // Refresh history while a backup is running.
  useEffect(() => {
    if (!runs.data?.some((r) => r.status === "running")) return;
    const id = setInterval(runs.reload, 3000);
    return () => clearInterval(id);
  }, [runs.data]); // eslint-disable-line react-hooks/exhaustive-deps

  if (!f) return <PageHead title={t("backup.title")} subtitle={t("backup.subtitle")} />;
  const set = (p: Partial<Backup>) => (setF({ ...f, ...p }), setDirty(true));
  const setT = (p: Partial<Target>) => set({ target: { ...f.target, ...p } });

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

  const save = (e: FormEvent) => {
    e.preventDefault();
    act(async () => {
      await api.put("/settings/backup", f);
      setF(await api.get<Backup>("/settings/backup"));
      setDirty(false);
    }, t("common.saved"));
  };

  const test = () =>
    act(async () => {
      await api.post("/backup/test");
      setF(await api.get<Backup>("/settings/backup"));
    }, t("backup.test_ok"));

  const run = (kind: "full" | "incremental") =>
    act(async () => {
      await api.post("/backup/run", { kind });
      setTimeout(runs.reload, 800);
    }, t("backup.started"));

  const loadRemote = () => act(async () => setRemote(await api.get<Remote[]>("/backup/remote")));
  const isFTP = f.target.type !== "scp";

  return (
    <>
      <PageHead title={t("backup.title")} subtitle={t("backup.subtitle")} />
      {msg && <Notice kind={msg.kind}>{msg.text}</Notice>}
      <form className="card" onSubmit={save}>
        <h2>{t("backup.location")}</h2>
        <div className="grid3">
          <div className="field">
            <label htmlFor="b-type">{t("backup.type")}</label>
            <select id="b-type" className="input" value={f.target.type} onChange={(e) => {
              const ty = e.target.value as Target["type"];
              setT({ type: ty, port: Object.values(defaultPorts).includes(f.target.port) || !f.target.port ? defaultPorts[ty] : f.target.port });
            }}>
              <option value="scp">{t("backup.type_scp")}</option>
              <option value="ftps-explicit">{t("backup.type_ftps_explicit")}</option>
              <option value="ftps-implicit">{t("backup.type_ftps_implicit")}</option>
              <option value="ftp">{t("backup.type_ftp")}</option>
            </select>
          </div>
          <div className="field">
            <label htmlFor="b-host">{t("backup.host")}</label>
            <input id="b-host" className="input" placeholder="backup.example.com" value={f.target.host} onChange={(e) => setT({ host: e.target.value })} />
          </div>
          <div className="field">
            <label htmlFor="b-port">{t("backup.port")}</label>
            <input id="b-port" className="input" type="number" min={1} max={65535} value={f.target.port || defaultPorts[f.target.type]} onChange={(e) => setT({ port: Number(e.target.value) })} />
          </div>
        </div>
        {f.target.type === "ftp" && <Notice kind="warn">{t("backup.ftp_warning")}</Notice>}
        <div className="grid3">
          <div className="field">
            <label htmlFor="b-user">{t("backup.username")}</label>
            <input id="b-user" className="input" autoComplete="off" value={f.target.username} onChange={(e) => setT({ username: e.target.value })} />
          </div>
          <div className="field">
            <label htmlFor="b-pass">{t("backup.password")}</label>
            <input id="b-pass" className="input" type="password" autoComplete="new-password" placeholder={f.has_password ? "••••••••" : ""} value={f.target.password ?? ""} onChange={(e) => setT({ password: e.target.value })} />
            {f.has_password && <span className="hint">{t("common.keep_secret")}</span>}
          </div>
          <div className="field">
            <label htmlFor="b-dir">{t("backup.dir")}</label>
            <input id="b-dir" className="input" value={f.target.dir} onChange={(e) => setT({ dir: e.target.value })} />
          </div>
        </div>
        {f.target.type === "scp" ? (
          <div className="grid2">
            <div className="field">
              <label htmlFor="b-key">{t("backup.private_key")}</label>
              <textarea id="b-key" className="input" placeholder={f.has_private_key ? t("common.secret_set") : "-----BEGIN OPENSSH PRIVATE KEY-----"} value={f.target.private_key ?? ""} onChange={(e) => setT({ private_key: e.target.value })} />
              <span className="hint">{t("backup.private_key_hint")}</span>
            </div>
            <div className="field">
              <label htmlFor="b-hostkey">{t("backup.host_key")}</label>
              <input id="b-hostkey" className="input mono" value={f.target.host_key} onChange={(e) => setT({ host_key: e.target.value })} />
              <span className="hint">{t("backup.host_key_hint")}</span>
            </div>
          </div>
        ) : (
          isFTP && f.target.type !== "ftp" && (
            <label className="check">
              <input id="b-skip" type="checkbox" checked={f.target.skip_tls_verify} onChange={(e) => setT({ skip_tls_verify: e.target.checked })} />
              <span>{t("backup.skip_verify")}</span>
            </label>
          )
        )}

        <h2 style={{ marginTop: 8 }}>{t("backup.schedule")}</h2>
        <div className="grid3">
          <div className="field">
            <label htmlFor="b-full">{t("backup.full_schedule")}</label>
            <input id="b-full" className="input mono" value={f.full_schedule} onChange={(e) => set({ full_schedule: e.target.value })} />
          </div>
          <div className="field">
            <label htmlFor="b-incr">{t("backup.incr_schedule")}</label>
            <input id="b-incr" className="input mono" value={f.incremental_schedule} onChange={(e) => set({ incremental_schedule: e.target.value })} />
          </div>
          <div className="field">
            <label htmlFor="b-keep">{t("backup.keep_full")}</label>
            <input id="b-keep" className="input" type="number" min={0} value={f.keep_full} onChange={(e) => set({ keep_full: Number(e.target.value) })} />
          </div>
        </div>
        <span className="hint">{t("backup.schedule_hint")} {t("backup.keep_full_hint")}</span>

        <h2 style={{ marginTop: 8 }}>{t("backup.encryption")}</h2>
        <div className="field" style={{ maxWidth: 420 }}>
          <label htmlFor="b-pp">{t("backup.passphrase")}</label>
          <input id="b-pp" className="input" type="password" autoComplete="new-password" placeholder={f.has_passphrase ? "••••••••" : ""} value={f.passphrase ?? ""} onChange={(e) => set({ passphrase: e.target.value })} />
          <span className="hint">{t("backup.passphrase_hint")}</span>
        </div>

        <div className="row">
          <button className="btn btn-primary" disabled={busy || !dirty}>
            {t("common.save")}
          </button>
          <button type="button" className="btn btn-secondary" onClick={test} disabled={busy || dirty || !f.target.host}>
            {t("backup.test")}
          </button>
        </div>
      </form>

      <div className="card">
        <div className="card-head">
          <h2>{t("backup.history")}</h2>
          <div className="row">
            <button className="btn btn-secondary btn-sm" onClick={() => run("incremental")} disabled={busy || dirty || !f.target.host}>
              {t("backup.run_incr")}
            </button>
            <button className="btn btn-primary btn-sm" onClick={() => run("full")} disabled={busy || dirty || !f.target.host}>
              {t("backup.run_full")}
            </button>
          </div>
        </div>
        {runs.data && runs.data.length === 0 ? (
          <p className="muted">{t("backup.none")}</p>
        ) : (
          <div className="tbl-wrap">
            <table>
              <thead>
                <tr>
                  <th>{t("backup.started_at")}</th>
                  <th>{t("backup.kind")}</th>
                  <th>{t("backup.size")}</th>
                  <th>{t("devices.status")}</th>
                </tr>
              </thead>
              <tbody>
                {(runs.data ?? []).map((r) => (
                  <tr key={r.id}>
                    <td>
                      {formatDate(r.started_at, i18n.language)}
                      <span className="sub mono">{r.name}</span>
                    </td>
                    <td>{t("backup." + r.kind)}</td>
                    <td>{formatBytes(r.size_bytes)}</td>
                    <td>
                      <span className={"pill " + (r.status === "ok" ? "ok" : r.status === "failed" ? "off" : "warn")}>{t("status." + r.status)}</span>
                      {r.error && <span className="sub">{r.error}</span>}
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
          <h2>{t("backup.remote")}</h2>
          <button className="btn btn-secondary btn-sm" onClick={loadRemote} disabled={busy || dirty || !f.target.host}>
            {t("backup.load_remote")}
          </button>
        </div>
        <p className="hint">{t("backup.remote_hint")}</p>
        {remote && remote.length === 0 && <p className="muted">{t("backup.none")}</p>}
        {remote && remote.length > 0 && (
          <div className="tbl-wrap">
            <table>
              <thead>
                <tr>
                  <th>{t("backup.name")}</th>
                  <th>{t("backup.size")}</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {remote.map((r) => (
                  <tr key={r.name}>
                    <td className="mono">{r.name}</td>
                    <td>{formatBytes(r.size)}</td>
                    <td style={{ textAlign: "right" }}>
                      <button className="btn btn-secondary btn-sm" onClick={() => setRestore(r.name)}>
                        <Icon name="restore" />
                        {t("backup.restore")}
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
      {restore && <RestoreDialog name={restore} onClose={() => setRestore(null)} onDone={() => setUser(null)} />}
    </>
  );
}

function RestoreDialog({ name, onClose, onDone }: { name: string; onClose: () => void; onDone: () => void }) {
  const { t } = useTranslation();
  const [confirm, setConfirm] = useState("");
  const [state, setState] = useState<"ask" | "running" | "done" | "failed">("ask");
  const [err, setErr] = useState("");

  const start = async (e: FormEvent) => {
    e.preventDefault();
    try {
      await api.post("/backup/restore", { name, confirm });
      setState("running");
      for (;;) {
        await new Promise((r) => setTimeout(r, 2000));
        const s = await api.get<{ running: boolean; error: string; done: boolean }>("/backup/restore").catch(() => null);
        if (s && s.done) {
          if (s.error) {
            setErr(s.error);
            setState("failed");
          } else {
            setState("done");
            setTimeout(onDone, 2500);
          }
          return;
        }
      }
    } catch (e) {
      setErr(errorText(t, e));
    }
  };

  return (
    <Dialog title={t("backup.restore_title", { name })} onClose={state === "running" ? () => {} : onClose}>
      {state === "ask" && (
        <form onSubmit={start} style={{ display: "grid", gap: 14 }}>
          <Notice kind="warn">{t("backup.restore_text")}</Notice>
          {err && <Notice kind="error">{err}</Notice>}
          <div className="field">
            <label htmlFor="r-confirm">{t("backup.restore_type")}</label>
            <input id="r-confirm" className="input mono" autoComplete="off" value={confirm} onChange={(e) => setConfirm(e.target.value)} />
          </div>
          <div className="row">
            <button className="btn btn-danger" disabled={confirm !== "RESTORE"}>
              {t("backup.restore")}
            </button>
            <button type="button" className="btn btn-secondary" onClick={onClose}>
              {t("common.cancel")}
            </button>
          </div>
        </form>
      )}
      {state === "running" && <Notice>{t("backup.restore_running", { name })}</Notice>}
      {state === "done" && <Notice kind="ok">{t("backup.restore_done")}</Notice>}
      {state === "failed" && <Notice kind="error">{t("backup.restore_failed", { error: err })}</Notice>}
    </Dialog>
  );
}
