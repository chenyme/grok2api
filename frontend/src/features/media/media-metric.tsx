import type { LucideIcon } from "lucide-react";

import { Spinner } from "@/components/ui/spinner";

type MediaMetricProps = {
  icon: LucideIcon;
  label: string;
  value: string;
  detail?: string;
  loading: boolean;
};

export function MediaMetric({ icon: Icon, label, value, detail, loading }: MediaMetricProps) {
  return (
    <div className="min-h-24 rounded-lg bg-card p-4">
      <div className="flex items-center justify-between gap-3">
        <span className="text-xs text-muted-foreground">{label}</span>
        <Icon className="size-4 shrink-0 text-muted-foreground" />
      </div>
      <div className="mt-3 flex min-h-7 items-center text-xl font-medium tabular-nums">{loading ? <Spinner /> : value}</div>
      {detail ? <p className="mt-1 text-xs text-muted-foreground">{detail}</p> : null}
    </div>
  );
}
