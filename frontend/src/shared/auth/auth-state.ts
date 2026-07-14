import { createContext } from "react";

import type { AdminDTO } from "@/shared/api/client";

export type AuthStatus = "restoring" | "authenticated" | "anonymous" | "unavailable";

export type AuthContextValue = {
  admin: AdminDTO | null;
  status: AuthStatus;
  retryRestore: () => Promise<void>;
  login: (username: string, password: string) => Promise<void>;
  logout: () => Promise<void>;
  changePassword: (currentPassword: string, newPassword: string) => Promise<void>;
};

export const AuthContext = createContext<AuthContextValue | null>(null);
