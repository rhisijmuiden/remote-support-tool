// Small fetch wrapper for the server API. Errors carry a stable code the UI translates.
export class ApiError extends Error {
  constructor(public status: number, public code: string, public detail?: string) {
    super(code);
  }
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch("/api" + path, {
    method,
    credentials: "same-origin",
    headers: {
      "X-Requested-With": "rhis-remote",
      ...(body !== undefined ? { "Content-Type": "application/json" } : {}),
    },
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  if (res.status === 204 || res.status === 202) return undefined as T;
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new ApiError(res.status, data.error ?? "internal", data.detail);
  return data as T;
}

export const api = {
  get: <T,>(p: string) => request<T>("GET", p),
  post: <T,>(p: string, b: unknown = {}) => request<T>("POST", p, b),
  put: <T,>(p: string, b: unknown) => request<T>("PUT", p, b),
  del: <T,>(p: string) => request<T>("DELETE", p),
};

export type User = {
  id: number;
  email: string;
  name: string;
  role: "admin" | "technician";
  language: "en" | "nl";
  created_at: string;
  last_login_at: string | null;
};

export type Meta = { product_name: string; company_name: string; setup_required: boolean; version: string };

export type BackupRun = {
  id: number;
  kind: "full" | "incremental";
  name: string;
  parent_name: string | null;
  target: string;
  status: "running" | "ok" | "failed";
  size_bytes: number | null;
  error: string | null;
  started_at: string;
  finished_at: string | null;
};

export type Release = { version: string; name: string; notes: string; url: string; published_at: string; prerelease: boolean };
export type UpdateStatus = {
  current: string;
  channel: string;
  build_channel: string;
  latest: Release | null;
  available: boolean;
  is_downgrade: boolean;
  checked_at: string;
  check_error?: string;
};

/** upload sends a file as the raw request body (logos, agent executables). */
export async function upload<T>(method: "PUT" | "POST", path: string, body: Blob, contentType: string): Promise<T> {
  const res = await fetch("/api" + path, {
    method,
    credentials: "same-origin",
    headers: { "X-Requested-With": "rhis-remote", "Content-Type": contentType },
    body,
  });
  if (res.status === 204) return undefined as T;
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new ApiError(res.status, data.error ?? "internal", data.detail);
  return data as T;
}

export type Customer = { id: number; name: string; devices: number; online: number };

export type Channel = "production" | "test";
export type BuildLink = {
  id: number;
  file_id: number;
  expires_at: string;
  max_downloads: number | null;
  downloads: number;
  revoked: boolean;
  active: boolean;
  created_at: string;
};
export type BuildFile = {
  id: number;
  build_id: number;
  agent_version: string;
  arch: "amd64" | "arm64";
  file_name: string;
  size_bytes: number;
  sha256: string;
  signed: boolean;
  created_at: string;
  links: BuildLink[];
};
export type Build = {
  id: number;
  name: string;
  customer_id: number | null;
  customer: string | null;
  mode: "install" | "quick";
  product_name: string;
  company_name: string;
  accent_color: string;
  has_logo: boolean;
  servers: string[];
  channel: Channel;
  revoked: boolean;
  created_at: string;
  updated_at: string;
  files?: BuildFile[];
};
export type AgentRelease = {
  id: number;
  product: "agent" | "quick";
  version: string;
  os: string;
  arch: "amd64" | "arm64";
  channel: Channel;
  size_bytes: number;
  sha256: string;
  source: "github" | "upload";
  notes: string;
  released_at: string;
  created_at: string;
};
export type UpdatePolicy = { enabled: boolean; production_version: string; test_version: string; rollout_percent: number };
export type CustomerOverride = { customer_id: number; customer: string; mode: "off" | "pin"; version: string; updated_at: string };
export type AgentOverview = {
  public_key: string;
  fingerprint: string;
  versions: { version: string; devices: number; online: number }[];
  events: {
    at: string;
    device_id: number | null;
    hostname: string | null;
    from_version: string | null;
    to_version: string;
    status: "offered" | "downloaded" | "installed" | "failed";
    error: string | null;
  }[];
};
export type CodesignStatus = {
  mode: "" | "pfx" | "command";
  command: string;
  timestamp_url: string;
  has_pfx: boolean;
  subject: string;
  issuer: string;
  not_after: string;
  thumbprint: string;
  tool: string;
  ready: boolean;
};
export type RecordingStatus = "none" | "recording" | "complete" | "incomplete" | "deleted";
export type RemoteSession = {
  id: number;
  device_id: number | null;
  device_name: string | null;
  customer_id: number | null;
  customer_name: string | null;
  user_id: number | null;
  user_email: string | null;
  user_name: string | null;
  kind: "unattended" | "attended" | "quick";
  transport: string | null;
  viewer_ip: string | null;
  started_at: string;
  ended_at: string | null;
  end_reason: string | null;
  recording: RecordingStatus;
  recording_chunks: number;
  recording_bytes: number;
  recording_mime: string | null;
};
export type SessionEvent = { id: number; at: string; kind: string; details: Record<string, unknown> | null };
export type Share = {
  id: number;
  session_id: number;
  created_by: string | null;
  note: string | null;
  created_at: string;
  expires_at: string;
  revoked_at: string | null;
  views: number;
  last_viewed_at: string | null;
};
export type RecordingPolicy = { mode: "off" | "optional" | "always"; retention_days: number; share_days: number };

export function hasRecording(s: RemoteSession) {
  return s.recording_chunks > 0 && (s.recording === "complete" || s.recording === "incomplete" || s.recording === "recording");
}

export function duration(s: { started_at: string; ended_at: string | null }, now = Date.now()) {
  const sec = Math.max(0, Math.round(((s.ended_at ? Date.parse(s.ended_at) : now) - Date.parse(s.started_at)) / 1000));
  const h = Math.floor(sec / 3600);
  const m = Math.floor((sec % 3600) / 60);
  const ss = sec % 60;
  return h > 0 ? `${h}:${String(m).padStart(2, "0")}:${String(ss).padStart(2, "0")}` : `${m}:${String(ss).padStart(2, "0")}`;
}
