import { Navigate, createBrowserRouter } from "react-router-dom";

import { AnonymousBoundary, AuthBoundary } from "@/app/auth-boundary";
import { DeferredAccountsPage, DeferredApiDocsPage, DeferredAppShell, DeferredClientKeysPage, DeferredCreativeConsolePage, DeferredDashboardPage, DeferredGalleryPage, DeferredModelsPage, DeferredRequestAuditsPage, DeferredSettingsPage, DeferredVideoGalleryPage } from "@/app/deferred-pages";
import { LoginPage } from "@/features/auth/login-page";

export const router = createBrowserRouter([
  {
    element: <AnonymousBoundary />,
    children: [{ path: "/login", element: <LoginPage /> }],
  },
  {
    element: <AuthBoundary />,
    children: [
      {
        element: <DeferredAppShell />,
        children: [
          { index: true, element: <Navigate to="/dashboard" replace /> },
          { path: "/dashboard", element: <DeferredDashboardPage /> },
          { path: "/accounts", element: <DeferredAccountsPage /> },
          { path: "/models", element: <DeferredModelsPage /> },
          { path: "/creative-console", element: <DeferredCreativeConsolePage /> },
          { path: "/client-keys", element: <DeferredClientKeysPage /> },
          { path: "/gallery", element: <DeferredGalleryPage /> },
          { path: "/video-gallery", element: <DeferredVideoGalleryPage /> },
          { path: "/request-audits", element: <DeferredRequestAuditsPage /> },
          { path: "/docs", element: <Navigate to="/docs/chat/completions" replace /> },
          { path: "/docs/:category/:endpoint", element: <DeferredApiDocsPage /> },
          { path: "/settings", element: <DeferredSettingsPage /> },
        ],
      },
    ],
  },
  { path: "*", element: <Navigate to="/dashboard" replace /> },
]);
