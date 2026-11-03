import { createContext, useContext } from "react";
import type { Meta, User } from "./api";

export type Session = { user: User; meta: Meta; setUser: (u: User | null) => void };
export const SessionContext = createContext<Session | null>(null);

export function useSession(): Session {
  const s = useContext(SessionContext);
  if (!s) throw new Error("no session");
  return s;
}
