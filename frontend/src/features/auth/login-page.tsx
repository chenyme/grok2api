import { zodResolver } from "@hookform/resolvers/zod";
import { useForm, type FieldErrors } from "react-hook-form";
import { useTranslation } from "react-i18next";
import { useNavigate } from "react-router-dom";
import { toast } from "sonner";
import { z } from "zod";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { useAuth } from "@/shared/auth/use-auth";
import { GitHubMark } from "@/shared/components/github-mark";
import { SiteFooter } from "@/shared/components/site-footer";

export function LoginPage() {
  const { t } = useTranslation();
  const { login } = useAuth();
  const navigate = useNavigate();
  const schema = z.object({
    username: z.string().min(1, t("auth.usernameRequired")),
    password: z.string().min(1, t("auth.passwordRequired")),
  });
  type LoginForm = z.infer<typeof schema>;
  const form = useForm<LoginForm>({
    resolver: zodResolver(schema),
    defaultValues: { username: "", password: "" },
  });

  async function submit(values: LoginForm): Promise<void> {
    try {
      await login(values.username, values.password);
      navigate("/dashboard", { replace: true });
    } catch (error) {
      toast.error(error instanceof Error ? error.message : t("errors.generic"));
      form.setFocus("password");
    }
  }

  function invalid(errors: FieldErrors<LoginForm>): void {
    if (errors.username) {
      toast.error(errors.username.message);
      form.setFocus("username");
      return;
    }
    if (errors.password) {
      toast.error(errors.password.message);
      form.setFocus("password");
    }
  }

  return (
    <div className="flex min-h-screen flex-col bg-background">
      <header className="mx-auto flex h-16 w-full max-w-[960px] items-center justify-between px-5 sm:px-8 lg:px-0">
        <span className="text-sm font-semibold text-foreground">{t("appName")}</span>
        <Button variant="ghost" size="icon" className="size-8 text-muted-foreground hover:text-foreground" asChild>
          <a href="https://github.com/chenyme/grok2api" target="_blank" rel="noreferrer" aria-label="GitHub">
            <GitHubMark />
          </a>
        </Button>
      </header>

      <main className="mx-auto flex w-full max-w-[960px] flex-1 items-center justify-center px-5 py-12 sm:px-8 lg:px-0">
        <div className="grid w-full max-w-[840px] -translate-y-6 items-center lg:-translate-y-10 lg:grid-cols-[minmax(0,1fr)_1px_336px] lg:gap-14">
          <section className="hidden min-h-72 flex-col justify-center lg:flex">
            <p className="text-xs font-medium text-muted-foreground">{t("appName")}</p>
            <h2 className="mt-3 max-w-sm text-3xl font-medium leading-tight">{t("auth.productTitle")}</h2>
            <p className="mt-4 max-w-xs text-xs leading-6 text-muted-foreground">{t("auth.subtitle")}</p>
          </section>

          <div className="hidden h-64 bg-border lg:block" aria-hidden="true" />

          <section className="w-full max-w-[336px] justify-self-center lg:justify-self-auto">
          <div className="mb-6">
            <h1 className="text-xl font-medium">{t("auth.title")}</h1>
            <p className="mt-2 text-xs leading-5 text-muted-foreground lg:hidden">{t("auth.subtitle")}</p>
          </div>
          <form className="space-y-4" onSubmit={form.handleSubmit(submit, invalid)}>
            <div className="space-y-2">
              <Label htmlFor="username">{t("auth.username")}</Label>
              <Input id="username" className="h-9 bg-card" autoComplete="username" autoFocus aria-invalid={Boolean(form.formState.errors.username)} {...form.register("username")} />
            </div>
            <div className="space-y-2">
              <Label htmlFor="password">{t("auth.password")}</Label>
              <Input id="password" className="h-9 bg-card" type="password" autoComplete="current-password" aria-invalid={Boolean(form.formState.errors.password)} {...form.register("password")} />
            </div>
            <Button type="submit" size="sm" className="w-full" disabled={form.formState.isSubmitting}>
              {form.formState.isSubmitting ? t("auth.signingIn") : t("auth.signIn")}
            </Button>
          </form>
          </section>
        </div>
      </main>
      <SiteFooter />
    </div>
  );
}
