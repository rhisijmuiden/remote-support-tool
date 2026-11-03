// Line icons (24px grid, stroke follows currentColor).
const paths: Record<string, string> = {
  dashboard: "M3 3h7v9H3zM14 3h7v5h-7zM14 12h7v9h-7zM3 16h7v5H3z",
  devices: "M3 4h18v12H3zM8 20h8M12 16v4",
  customers: "M4 21V8l8-5 8 5v13M9 21v-6h6v6",
  users: "M16 11a4 4 0 1 0-8 0M4 21a8 8 0 0 1 16 0",
  mail: "M3 5h18v14H3zM3 7l9 6 9-6",
  backup: "M4 7c0-2.7 16-2.7 16 0v10c0 2.7-16 2.7-16 0zM4 7c0 2.7 16 2.7 16 0M4 12c0 2.7 16 2.7 16 0",
  audit: "M8 4h11v16H5V7zM8 4v3H5M9 11h6M9 15h6",
  info: "M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18zM12 11v6M12 7.5v.01",
  account: "M12 12a4 4 0 1 0 0-8 4 4 0 0 0 0 8zM4 21a8 8 0 0 1 16 0",
  logout: "M15 4h4v16h-4M10 8l-4 4 4 4M6 12h10",
  menu: "M4 6h16M4 12h16M4 18h16",
  quick: "M13 3L5 13h6l-1 8 8-10h-6z",
  download: "M12 4v11M7 10l5 5 5-5M5 20h14",
  close: "M6 6l12 12M18 6L6 18",
  sun: "M12 8a4 4 0 1 0 0 8 4 4 0 0 0 0-8zM12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4",
  moon: "M20 14.5A8 8 0 0 1 9.5 4 8 8 0 1 0 20 14.5z",
  trash: "M4 7h16M10 11v6M14 11v6M6 7l1 13h10l1-13M9 7V4h6v3",
  edit: "M4 20h4L19 9l-4-4L4 16zM13 7l4 4",
  key: "M15 7a4 4 0 1 1-3.9 5H3v4h3v-2h2v2h3.1A4 4 0 0 1 15 7z",
  restore: "M3 12a9 9 0 1 0 3-6.7M3 4v5h5",
  plus: "M12 5v14M5 12h14",
  connect: "M3 4h18v12H3zM8 20h8M12 16v4M9 10h6M12 7l3 3-3 3",
  back: "M15 5l-7 7 7 7",
  lock: "M6 11h12v9H6zM8.5 11V8a3.5 3.5 0 0 1 7 0v3",
  eye: "M2 12s3.5-6 10-6 10 6 10 6-3.5 6-10 6S2 12 2 12zM12 9.5a2.5 2.5 0 1 0 0 5 2.5 2.5 0 0 0 0-5z",
  fullscreen: "M4 9V4h5M15 4h5v5M20 15v5h-5M9 20H4v-5",
  copy: "M9 9h11v11H9zM5 15H4V4h11v1",
  auto: "M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18zM12 3v18",
};

export function Icon({ name }: { name: keyof typeof paths | string }) {
  return (
    <svg viewBox="0 0 24 24" aria-hidden="true">
      <path d={paths[name] ?? ""} />
    </svg>
  );
}
