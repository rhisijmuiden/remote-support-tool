import { ReactNode, useEffect, useState } from "react";
import { NavLink, useLocation } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { api } from "../api";
import { setLanguage } from "../i18n";
import { useSession } from "../session";
import { Icon } from "./Icon";
import { Logo } from "./Logo";
import { InfoDialog } from "./InfoDialog";

type Theme = "system" | "light" | "dark";

function applyTheme(th: Theme) {
  if (th === "system") document.documentElement.removeAttribute("data-theme");
  else document.documentElement.setAttribute("data-theme", th);
}

function savedTheme(): Theme {
  try {
    const v = localStorage.getItem("theme");
    if (v === "light" || v === "dark") return v;
  } catch {
    /* ignore */
  }
  return "system";
}

export function Layout({ children }: { children: ReactNode }) {
  const { t, i18n } = useTranslation();
  const { user, meta, setUser } = useSession();
  const [open, setOpen] = useState(false);
  const [info, setInfo] = useState(false);
  const [theme, setTheme] = useState<Theme>(savedTheme);
  const loc = useLocation();
  const admin = user.role === "admin";

  useEffect(() => setOpen(false), [loc.pathname]);
  useEffect(() => {
    applyTheme(theme);
    try {
      localStorage.setItem("theme", theme);
    } catch {
      /* ignore */
    }
  }, [theme]);

  const nav = (to: string, icon: string, label: string) => (
    <NavLink to={to} end={to === "/"} className={({ isActive }) => "nav" + (isActive ? " active" : "")}>
      <Icon name={icon} />
      <span>{label}</span>
    </NavLink>
  );

  const switchLang = (l: "en" | "nl") => {
    setLanguage(l);
    api.put("/me/language", { language: l }).catch(() => {});
  };

  const logout = async () => {
    await api.post("/logout").catch(() => {});
    setUser(null);
  };

  const nextTheme: Record<Theme, Theme> = { system: "dark", dark: "light", light: "system" };

  return (
    <div className={"shell" + (open ? " open" : "")}>
      <div className="mobile-bar">
        <span className="brand" style={{ padding: 0 }}>
          <Logo />
          <small>Remote</small>
        </span>
        <button className="btn-icon" onClick={() => setOpen(!open)} aria-label={t("nav.menu")} aria-expanded={open}>
          <Icon name={open ? "close" : "menu"} />
        </button>
      </div>
      <aside className="side">
        <div className="brand">
          <Logo />
          <small>Remote</small>
        </div>
        {nav("/", "dashboard", t("nav.dashboard"))}
        {nav("/quick", "quick", t("nav.quick"))}
        {nav("/devices", "devices", t("nav.devices"))}
        {nav("/customers", "customers", t("nav.customers"))}
        {admin && (
          <>
            <div className="grp">{t("nav.group_settings")}</div>
            {nav("/settings/users", "users", t("nav.users"))}
            {nav("/settings/mail", "mail", t("nav.mail"))}
            {nav("/settings/backup", "backup", t("nav.backup"))}
            {nav("/settings/audit", "audit", t("nav.audit"))}
          </>
        )}
        <div className="spacer" />
        <div className="side-foot">
          <button className="nav" onClick={() => setInfo(true)}>
            <Icon name="info" />
            <span>{t("nav.info")}</span>
            <span className="ver">{meta.version}</span>
          </button>
          {nav("/account", "account", t("nav.account"))}
          <div className="user-row">
            <span className="who muted" title={user.email}>
              {user.name}
            </span>
            <span className="controls">
              <span className="seg" role="group" aria-label={t("nav.language")}>
                <button aria-pressed={i18n.language === "en"} onClick={() => switchLang("en")}>
                  EN
                </button>
                <button aria-pressed={i18n.language === "nl"} onClick={() => switchLang("nl")}>
                  NL
                </button>
              </span>
              <button
                className="btn-icon"
                onClick={() => setTheme(nextTheme[theme])}
                aria-label={`${t("nav.theme")}: ${t("common." + theme)}`}
                title={`${t("nav.theme")}: ${t("common." + theme)}`}
              >
                <Icon name={theme === "dark" ? "moon" : theme === "light" ? "sun" : "auto"} />
              </button>
              <button className="btn-icon" onClick={logout} aria-label={t("nav.logout")} title={t("nav.logout")}>
                <Icon name="logout" />
              </button>
            </span>
          </div>
        </div>
      </aside>
      <main className="main">{children}</main>
      {info && <InfoDialog onClose={() => setInfo(false)} />}
    </div>
  );
}
