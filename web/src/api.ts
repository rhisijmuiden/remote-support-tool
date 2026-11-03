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
