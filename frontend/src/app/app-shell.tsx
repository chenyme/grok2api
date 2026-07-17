import { zodResolver } from "@hookform/resolvers/zod";
import { Box, ChevronDown, Eye, Image, KeyRound, Languages, LayoutDashboard, LogOut, Menu, MessageSquareText, Monitor, Moon, MoreHorizontal, Settings, Sparkles, Sun, Users, Video } from "lucide-react";
import { useTheme } from "next-themes";
import { useState, type ReactNode } from "react";
import { useForm } from "react-hook-form";
import { useTranslation } from "react-i18next";
import { Link, NavLink, Outlet, useLocation } from "react-router-dom";
import { toast } from "sonner";
import { z } from "zod";

import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuSub, DropdownMenuSubContent, DropdownMenuSubTrigger, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle, SheetTrigger } from "@/components/ui/sheet";
import { useAuth } from "@/shared/auth/use-auth";
import { GitHubMark } from "@/shared/components/github-mark";
import { SiteFooter } from "@/shared/components/site-footer";
import { cn } from "@/shared/lib/cn";
import { CurrentVersionLabel } from "@/features/system/version-update";

const navigation = [
  { href: "/dashboard", label: "nav.dashboard", icon: LayoutDashboard },
  { href: "/accounts", label: "nav.accounts", icon: Users },
  { href: "/client-keys", label: "nav.clientKeys", icon: KeyRound },
  { href: "/models", label: "nav.models", icon: Box },
  { href: "/gallery", label: "nav.gallery", icon: Image },
  { href: "/video-gallery", label: "nav.videoGallery", icon: Video },
  { href: "/request-audits", label: "nav.audits", icon: Eye },
  { href: "/creative-console", label: "nav.creativeConsole", icon: Sparkles },
] as const;

const documentation = [
  {
    label: "Chat",
    icon: MessageSquareText,
    items: [
      { href: "/docs/chat/completions", label: "Chat Completions", method: "POST" },
      { href: "/docs/chat/responses", label: "Responses", method: "POST" },
      { href: "/docs/chat/messages", label: "Messages", method: "POST" },
    ],
  },
  {
    label: "Image",
    icon: Image,
    items: [
      { href: "/docs/image/generations", label: "Image Generations", method: "POST" },
      { href: "/docs/image/edits", label: "Image Edits", method: "POST" },
    ],
  },
  {
    label: "Video",
    icon: Video,
    items: [
      { href: "/docs/video/generations", label: "Video Generations", method: "POST" },
      { href: "/docs/video/get", label: "Get Video", method: "GET" },
    ],
  },
] as const;

export function AppShell() {
  const { t, i18n } = useTranslation();
  const { admin, logout, changePassword } = useAuth();
  const location = useLocation();
  const { setTheme } = useTheme();
  const [mobileOpen, setMobileOpen] = useState(false);
  const [passwordOpen, setPasswordOpen] = useState(false);
  const [documentationOpen, setDocumentationOpen] = useState<Record<string, boolean>>({});
  const isMediaWorkspace = ["/creative-console", "/gallery", "/video-gallery"].includes(location.pathname);

  const passwordSchema = z.object({
    currentPassword: z.string().min(1, t("errors.required")),
    newPassword: z.string().min(8, t("errors.minPassword")),
  });
  type PasswordForm = z.infer<typeof passwordSchema>;
  const passwordForm = useForm<PasswordForm>({
    resolver: zodResolver(passwordSchema),
    defaultValues: { currentPassword: "", newPassword: "" },
  });

  async function submitPassword(values: PasswordForm): Promise<void> {
    try {
      await changePassword(values.currentPassword, values.newPassword);
      toast.success(t("auth.passwordUpdated"));
      passwordForm.reset();
      setPasswordOpen(false);
      await logout();
    } catch (error) {
      toast.error(error instanceof Error ? error.message : t("errors.generic"));
    }
  }

  function navigationLinks(): ReactNode {
    return navigation.map(({ href, label, icon: Icon }) => (
      <NavLink
        key={href}
        to={href}
        onClick={() => setMobileOpen(false)}
        className={({ isActive }) => cn(
          "group flex h-8 items-center gap-2 rounded-md px-2.5 text-xs font-normal text-muted-foreground transition-colors hover:bg-secondary/55 hover:text-foreground",
          isActive && "bg-secondary/60 text-foreground",
        )}
      >
        {({ isActive }) => (
          <>
            <span className="flex size-5 shrink-0 items-center justify-center">
              <Icon className={cn("size-4 text-muted-foreground", isActive && "text-foreground")} fill={isActive ? "currentColor" : "none"} fillOpacity={isActive ? 0.14 : 0} strokeWidth={1.8} />
            </span>
            {t(label)}
          </>
        )}
      </NavLink>
    ));
  }

  function documentationLinks(): ReactNode {
    return documentation.map(({ label, icon: Icon, items }) => {
      const open = documentationOpen[label] ?? false;
      return (
        <div key={label}>
          <button
            type="button"
            className="flex h-8 w-full items-center gap-2 rounded-md px-2.5 text-xs font-normal text-muted-foreground transition-colors hover:bg-secondary/55 hover:text-foreground"
            aria-expanded={open}
            onClick={() => setDocumentationOpen((current) => ({ ...current, [label]: !open }))}
          >
            <span className="flex size-5 shrink-0 items-center justify-center">
              <Icon className="size-[15px] text-muted-foreground" strokeWidth={1.7} />
            </span>
            <span className="flex-1 text-left">{label}</span>
            <ChevronDown className={cn("size-3 text-muted-foreground transition-transform", !open && "-rotate-90")} />
          </button>
          <div className={cn(
            "grid transition-[grid-template-rows,opacity] duration-200 ease-out",
            open ? "grid-rows-[1fr] opacity-100" : "pointer-events-none grid-rows-[0fr] opacity-0",
          )} aria-hidden={!open}>
            <div className="overflow-hidden">
              <div className="space-y-1 pt-1">
                {items.map((item) => (
                  <NavLink
                    key={item.href}
                    to={item.href}
                    onClick={() => setMobileOpen(false)}
                    className={({ isActive }) => cn(
                      "group flex h-7 min-w-0 items-center gap-2 rounded-md pl-[38px] pr-2.5 text-xs text-muted-foreground transition-colors hover:bg-secondary/55 hover:text-foreground",
                      isActive && "bg-secondary/60 text-foreground",
                    )}
                  >
                    <span className="min-w-0 flex-1 truncate">{item.label}</span>
                    <span className={cn(
                      "shrink-0 font-mono text-[9px] font-medium text-muted-foreground/70",
                      item.method === "GET" && "text-emerald-600 dark:text-emerald-400",
                      item.method === "POST" && "text-sky-600 dark:text-sky-400",
                    )}>
                      {item.method}
                    </span>
                  </NavLink>
                ))}
              </div>
            </div>
          </div>
        </div>
      );
    });
  }

  const navigationContent = (
    <nav className="mt-7 min-h-0 flex-1 overflow-y-auto overscroll-contain pr-2 pb-2" aria-label={t("shell.navigation")}>
      <div className="space-y-1">{navigationLinks()}</div>
      <div className="mt-7">
        <div className="px-2.5 pb-2 text-xs font-normal text-foreground">{t("nav.docs")}</div>
        <div className="space-y-1">{documentationLinks()}</div>
      </div>
    </nav>
  );

  const accountControl = (
    <div className="flex h-9 items-center gap-1 px-2.5">
      <span className="min-w-0 flex-1 truncate text-xs font-normal capitalize text-muted-foreground">{admin?.username}</span>
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button variant="ghost" size="icon" className="size-7 shrink-0 text-muted-foreground hover:text-foreground" aria-label={t("common.actions")}><MoreHorizontal /></Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end" side="top" sideOffset={8} className="w-56 p-1.5">
          <DropdownMenuSub>
            <DropdownMenuSubTrigger className="h-8"><Sun />{t("shell.appearance")}</DropdownMenuSubTrigger>
            <DropdownMenuSubContent>
              <DropdownMenuItem onClick={() => setTheme("light")}><Sun />{t("shell.light")}</DropdownMenuItem>
              <DropdownMenuItem onClick={() => setTheme("dark")}><Moon />{t("shell.dark")}</DropdownMenuItem>
              <DropdownMenuItem onClick={() => setTheme("system")}><Monitor />{t("shell.system")}</DropdownMenuItem>
            </DropdownMenuSubContent>
          </DropdownMenuSub>
          <DropdownMenuSub>
            <DropdownMenuSubTrigger className="h-8"><Languages />{t("shell.language")}</DropdownMenuSubTrigger>
            <DropdownMenuSubContent>
              <DropdownMenuItem onClick={() => void i18n.changeLanguage("zh-CN")}>简体中文</DropdownMenuItem>
              <DropdownMenuItem onClick={() => void i18n.changeLanguage("en")}>English</DropdownMenuItem>
            </DropdownMenuSubContent>
          </DropdownMenuSub>
          <DropdownMenuItem className="h-8" onClick={() => setPasswordOpen(true)}><KeyRound />{t("auth.changePassword")}</DropdownMenuItem>
          <DropdownMenuSeparator />
          <DropdownMenuItem className="h-8" onClick={() => void logout()}><LogOut />{t("auth.signOut")}</DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
      <NavLink
        to="/settings"
        onClick={() => setMobileOpen(false)}
        className={({ isActive }) => cn("flex size-7 shrink-0 items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-secondary/55 hover:text-foreground", isActive && "bg-secondary/60 text-foreground")}
        aria-label={t("nav.settings")}
      >
        <Settings className="size-4" strokeWidth={1.8} />
      </NavLink>
    </div>
  );

  return (
    <div className="min-h-screen bg-background">
        <aside className="fixed inset-y-0 left-0 z-30 hidden h-screen w-[288px] flex-col overflow-hidden bg-sidebar px-4 py-6 lg:flex">
          <div className="flex h-7 shrink-0 items-center justify-between px-2.5">
            <Link to="/dashboard" className="flex h-7 items-baseline gap-2 text-base font-semibold text-foreground">
              <span>{t("appName")}</span>
              <CurrentVersionLabel />
            </Link>
            <Button variant="ghost" size="icon" className="size-7 text-muted-foreground [&_svg]:size-[15px]" asChild>
              <a href="https://github.com/chenyme/grok2api" target="_blank" rel="noreferrer" aria-label="GitHub">
                <GitHubMark />
              </a>
            </Button>
          </div>
          {navigationContent}
          <div className="relative z-10 mt-4 shrink-0 bg-sidebar pt-4">{accountControl}</div>
        </aside>

        <div className="flex min-h-screen flex-col lg:pl-[288px]">
          <header className="flex h-12 items-center justify-between border-b px-4 lg:hidden">
            <Sheet open={mobileOpen} onOpenChange={setMobileOpen}>
              <SheetTrigger asChild><Button variant="ghost" size="icon" className="size-8" aria-label={t("shell.openNavigation")}><Menu className="size-4" /></Button></SheetTrigger>
              <SheetContent side="left" className="flex w-72 flex-col gap-0 bg-sidebar px-3 py-4 [&>button]:right-2 [&>button]:top-3.5 [&>button]:flex [&>button]:size-7 [&>button]:items-center [&>button]:justify-center [&>nav]:mt-5 [&>nav]:pr-1">
                <SheetHeader className="h-7 shrink-0 px-2.5 text-left">
                  <SheetTitle className="flex h-7 items-center text-base">{t("appName")}</SheetTitle>
                  <SheetDescription className="sr-only">{t("shell.navigation")}</SheetDescription>
                </SheetHeader>
                {navigationContent}
                <div className="relative z-10 mt-3 shrink-0 bg-sidebar pt-3">{accountControl}</div>
              </SheetContent>
            </Sheet>
            <span className="flex items-baseline gap-2 text-sm font-semibold"><span>{t("appName")}</span><CurrentVersionLabel /></span>
            <Button variant="ghost" size="icon" className="size-8 text-muted-foreground hover:text-foreground" asChild>
              <a href="https://github.com/chenyme/grok2api" target="_blank" rel="noreferrer" aria-label="GitHub">
                <GitHubMark />
              </a>
            </Button>
          </header>

          <main className={cn("mx-auto w-full max-w-[1280px] flex-1 px-5 sm:px-8", isMediaWorkspace ? "pt-8 pb-0 lg:pt-20" : "py-8 lg:py-20")}>
            <Outlet />
          </main>
          {!isMediaWorkspace ? <SiteFooter /> : null}
        </div>

      <Dialog open={passwordOpen} onOpenChange={setPasswordOpen}>
        <DialogContent>
          <DialogHeader><DialogTitle>{t("auth.changePassword")}</DialogTitle><DialogDescription>{admin?.username}</DialogDescription></DialogHeader>
          <form className="space-y-4" onSubmit={passwordForm.handleSubmit(submitPassword)}>
            <div className="space-y-2"><Label htmlFor="current-password">{t("auth.currentPassword")}</Label><Input id="current-password" type="password" autoComplete="current-password" {...passwordForm.register("currentPassword")} />{passwordForm.formState.errors.currentPassword ? <p className="text-xs text-destructive">{passwordForm.formState.errors.currentPassword.message}</p> : null}</div>
            <div className="space-y-2"><Label htmlFor="new-password">{t("auth.newPassword")}</Label><Input id="new-password" type="password" autoComplete="new-password" {...passwordForm.register("newPassword")} />{passwordForm.formState.errors.newPassword ? <p className="text-xs text-destructive">{passwordForm.formState.errors.newPassword.message}</p> : null}</div>
            <DialogFooter><Button type="button" variant="secondary" size="sm" onClick={() => setPasswordOpen(false)}>{t("common.cancel")}</Button><Button type="submit" size="sm" disabled={passwordForm.formState.isSubmitting}>{t("common.save")}</Button></DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
    </div>
  );
}
