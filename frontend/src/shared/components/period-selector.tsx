import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { PERIOD_DAYS, toPeriodValue, type PeriodDays } from "@/shared/lib/period";

export function PeriodSelector({ value, onChange, ariaLabel, className }: { value: PeriodDays; onChange: (value: PeriodDays) => void; ariaLabel: string; className?: string }) {
  return (
    <Tabs value={String(value)} onValueChange={(next) => { const days = Number(next) as PeriodDays; if (PERIOD_DAYS.includes(days)) onChange(days); }} className={className}>
      <TabsList aria-label={ariaLabel}>
      {PERIOD_DAYS.map((days) => (
        <TabsTrigger
          key={days}
          value={String(days)}
          className="min-w-9 px-1.5 font-normal sm:min-w-11 sm:px-2"
        >
          {toPeriodValue(days)}
        </TabsTrigger>
      ))}
      </TabsList>
    </Tabs>
  );
}
