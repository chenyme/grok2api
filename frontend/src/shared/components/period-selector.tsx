import { Button } from "@/components/ui/button";
import { cn } from "@/shared/lib/cn";
import { PERIOD_DAYS, toPeriodValue, type PeriodDays } from "@/shared/lib/period";

export function PeriodSelector({ value, onChange, ariaLabel, className }: { value: PeriodDays; onChange: (value: PeriodDays) => void; ariaLabel: string; className?: string }) {
  return (
    <div className={cn("inline-flex h-8 items-center gap-0.5 rounded-md bg-muted p-0.5", className)} role="group" aria-label={ariaLabel}>
      {PERIOD_DAYS.map((days) => (
        <Button
          key={days}
          type="button"
          variant="ghost"
          size="sm"
          className={cn("h-7 min-w-9 rounded-sm px-1.5 text-xs font-normal sm:min-w-11 sm:px-2", value === days && "bg-background shadow-sm hover:bg-background")}
          aria-pressed={value === days}
          onClick={() => onChange(days)}
        >
          {toPeriodValue(days)}
        </Button>
      ))}
    </div>
  );
}
