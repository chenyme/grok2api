import { useCallback, useEffect, useState, type ReactNode } from "react";

import {
  ApiError,
  apiRequest,
  refreshAccessToken,
  setAccessToken,
  subscribeSessionInvalidated,
  type AdminDTO,
  type LoginResponseDTO,
} from "@/shared/api/client";
import { AuthContext, type AuthStatus } from "@/shared/auth/auth-state";

export function AuthProvider({ children }: { children: ReactNode }) {
  const [admin, setAdmin] = useState<AdminDTO | null>(null);
  const [status, setStatus] = useState<AuthStatus>("restoring");

  const restoreSession = useCallback(async (): Promise<void> => {
    setStatus("restoring");
    const refreshResult = await refreshAccessToken();
    if (refreshResult === "invalid") {
      setAdmin(null);
      setStatus("anonymous");
      return;
    }
    if (refreshResult === "unavailable") {
      setStatus("unavailable");
      return;
    }

    try {
      const value = await apiRequest<AdminDTO>("/api/admin/v1/me", { retryAuth: false });
      setAdmin(value);
      setStatus("authenticated");
    } catch (error) {
      setAccessToken(null);
      setAdmin(null);
      setStatus(error instanceof ApiError && error.status === 401 ? "anonymous" : "unavailable");
    }
  }, []);

  useEffect(() => {
    const unsubscribe = subscribeSessionInvalidated(() => {
      setAdmin(null);
      setStatus("anonymous");
    });

    const restoreTimer = window.setTimeout(() => {
      void restoreSession();
    }, 0);

    return () => {
      window.clearTimeout(restoreTimer);
      unsubscribe();
    };
  }, [restoreSession]);

  async function login(username: string, password: string): Promise<void> {
    const response = await apiRequest<LoginResponseDTO>("/api/admin/v1/auth/login", {
      method: "POST",
      body: { username, password },
      authenticated: false,
      retryAuth: false,
    });
    setAccessToken(response.tokens.accessToken);
    setAdmin(response.admin);
    setStatus("authenticated");
  }

  async function logout(): Promise<void> {
    try {
      await apiRequest<{ loggedOut: boolean }>("/api/admin/v1/auth/logout", {
        method: "POST",
        body: {},
        authenticated: false,
        retryAuth: false,
      });
    } finally {
      setAccessToken(null);
      setAdmin(null);
      setStatus("anonymous");
    }
  }

  async function changePassword(currentPassword: string, newPassword: string): Promise<void> {
    await apiRequest("/api/admin/v1/me/password", {
      method: "PUT",
      body: { currentPassword, newPassword },
    });
  }

  return (
    <AuthContext.Provider value={{ admin, status, retryRestore: restoreSession, login, logout, changePassword }}>
      {children}
    </AuthContext.Provider>
  );
}
