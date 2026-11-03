import { useEffect, useState } from "react";
import { Navigate, Route, Routes, useLocation } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { api, ApiError, Meta, User } from "./api";
import { setLanguage } from "./i18n";
import { SessionContext } from "./session";
import { Layout } from "./components/Layout";
import { Login, Setup } from "./pages/Auth";
import { Dashboard } from "./pages/Dashboard";
import { Devices } from "./pages/Devices";
import { Customers } from "./pages/Customers";
import { Users } from "./pages/Users";
import { MailSettings } from "./pages/MailSettings";
import { BackupSettings } from "./pages/BackupSettings";
import { AuditLog } from "./pages/AuditLog";
import { Servers } from "./pages/Servers";
import { Account } from "./pages/Account";
import { BuildDetail, Builds } from "./pages/Builds";
import { AgentUpdates } from "./pages/AgentUpdates";
import { CodeSigning } from "./pages/CodeSigning";
import { Sessions } from "./pages/Sessions";
import { SessionDetail } from "./pages/SessionDetail";
import { RecordingSettings } from "./pages/RecordingSettings";
import { SharedRecording } from "./pages/SharedRecording";

export function App() {
  // Share links are public: they never ask for a sign-in.
  if (useLocation().pathname.startsWith("/r/")) {
    return (
      <Routes>
        <Route path="/r/:token" element={<SharedRecording />} />
      </Routes>
    );
  }
  return <Portal />;
}

function Portal() {
  const { t } = useTranslation();
  const [meta, setMeta] = useState<Meta | null>(null);
  const [user, setUser] = useState<User | null | undefined>(undefined);

  useEffect(() => {
    api.get<Meta>("/meta").then((m) => {
      setMeta(m);
      document.title = m.product_name;
    });
    api.get<User>("/me").then(setUser, (e) => {
      if (e instanceof ApiError && e.status === 401) setUser(null);
      else setUser(null);
    });
  }, []);

  useEffect(() => {
    if (user) setLanguage(user.language);
  }, [user]);

  if (!meta || user === undefined) return <div className="empty">{t("common.loading")}</div>;
  if (!user) {
    return meta.setup_required ? (
      <Setup meta={meta} onDone={(u) => (setMeta({ ...meta, setup_required: false }), setUser(u))} />
    ) : (
      <Login meta={meta} onDone={setUser} />
    );
  }
  const admin = user.role === "admin";
  return (
    <SessionContext.Provider value={{ user, meta, setUser }}>
      <Layout>
        <Routes>
          <Route path="/" element={<Dashboard />} />
          <Route path="/devices" element={<Devices />} />
          <Route path="/customers" element={<Customers />} />
          <Route path="/builds" element={<Builds />} />
          <Route path="/builds/:id" element={<BuildDetail />} />
          <Route path="/sessions" element={<Sessions />} />
          <Route path="/sessions/:id" element={<SessionDetail />} />
          <Route path="/account" element={<Account />} />
          {admin && <Route path="/settings/agent-updates" element={<AgentUpdates />} />}
          {admin && <Route path="/settings/codesign" element={<CodeSigning />} />}
          {admin && <Route path="/settings/users" element={<Users />} />}
          {admin && <Route path="/settings/mail" element={<MailSettings />} />}
          {admin && <Route path="/settings/backup" element={<BackupSettings />} />}
          {admin && <Route path="/settings/recording" element={<RecordingSettings />} />}
          {admin && <Route path="/settings/servers" element={<Servers />} />}
          {admin && <Route path="/settings/audit" element={<AuditLog />} />}
          <Route path="*" element={<Navigate to="/" replace />} />
        </Routes>
      </Layout>
    </SessionContext.Provider>
  );
}
