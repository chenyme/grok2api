import { format } from "date-fns"
import { enUS, zhCN } from "date-fns/locale"
import { CalendarIcon, X } from "lucide-react"
import { useRef } from "react"
import { useTranslation } from "react-i18next"
import { enUS as enUSDayPicker, zhCN as zhCNDayPicker } from "react-day-picker/locale"

import { Button } from "@/components/ui/button"
import { Calendar } from "@/components/ui/calendar"
import { Input } from "@/components/ui/input"
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover"
import { cn } from "@/shared/lib/cn"
import { toDateTimeLocal } from "@/shared/lib/format"

type DateTimePickerProps = {
  value: string
  onChange: (value: string) => void
  placeholder?: string
}

export function DateTimePicker({ value, onChange, placeholder }: DateTimePickerProps) {
  const { t, i18n } = useTranslation()
  const selected = parseLocalDateTime(value)
  const isChinese = i18n.language.toLowerCase().startsWith("zh")
  const timeParts = getTimeParts(selected)
  const timeInputRefs = useRef<Array<HTMLInputElement | null>>([])

  function enteredTimeParts(): TimeParts {
    return timeParts.map((part, index) => timeInputRefs.current[index]?.value ?? part) as TimeParts
  }

  function selectDate(date: Date | undefined): void {
    if (!date) {
      onChange("")
      return
    }
    const [hours, minutes, seconds] = normalizedTimeParts(enteredTimeParts())
    date.setHours(hours, minutes, seconds, 0)
    onChange(toDateTimeLocal(date.toISOString()))
  }

  function commitTime(): void {
    const normalized = normalizedTimeParts(enteredTimeParts())
    const date = selected ? new Date(selected) : new Date()
    date.setHours(normalized[0], normalized[1], normalized[2], 0)
    onChange(toDateTimeLocal(date.toISOString()))
  }

  return (
    <Popover>
      <div className="flex items-center gap-1.5">
        <PopoverTrigger asChild>
          <Button type="button" variant="outline" className={cn("h-8 min-w-0 flex-1 justify-start rounded-md bg-secondary/55 px-3 text-left font-normal", !selected && "text-muted-foreground")}>
            <CalendarIcon />
            <span className="truncate">
              {selected ? format(selected, isChinese ? "yyyy年M月d日 HH:mm:ss" : "MMM d, yyyy HH:mm:ss", { locale: isChinese ? zhCN : enUS }) : (placeholder ?? t("keys.neverExpires"))}
            </span>
          </Button>
        </PopoverTrigger>
        {selected ? <Button type="button" variant="ghost" size="icon" className="size-8 shrink-0" onClick={() => onChange("")} aria-label={t("keys.clearExpiry")} title={t("keys.clearExpiry")}><X /></Button> : null}
      </div>
      <PopoverContent className="w-auto overflow-hidden p-0" align="start">
        <Calendar
          mode="single"
          selected={selected}
          defaultMonth={selected}
          onSelect={selectDate}
          locale={isChinese ? zhCNDayPicker : enUSDayPicker}
        />
        <div className="flex items-center gap-3 border-t p-3">
          <span className="text-xs text-muted-foreground">{t("keys.expiryTime")}</span>
          <div className="ml-auto flex items-center gap-1 font-mono text-xs">
            {timeParts.map((part, index) => (
              <div className="contents" key={index}>
                {index > 0 ? <span className="text-muted-foreground">:</span> : null}
                <Input
                  aria-label={`${t("keys.expiryTime")} ${index === 0 ? "HH" : index === 1 ? "MM" : "SS"}`}
                  className="h-8 w-10 bg-background px-1 text-center font-mono text-xs tabular-nums"
                  inputMode="numeric"
                  maxLength={2}
                  pattern="[0-9]*"
                  defaultValue={part}
                  key={`${value}-${index}`}
                  ref={(element) => {
                    timeInputRefs.current[index] = element
                  }}
                  onChange={(event) => {
                    event.currentTarget.value = event.currentTarget.value.replace(/\D/g, "").slice(0, 2)
                  }}
                  onBlur={commitTime}
                  onFocus={(event) => event.currentTarget.select()}
                  onKeyDown={(event) => {
                    if (event.key === "Enter") {
                      commitTime()
                      event.currentTarget.blur()
                    }
                  }}
                />
              </div>
            ))}
          </div>
        </div>
      </PopoverContent>
    </Popover>
  )
}

type TimeParts = [string, string, string]

function getTimeParts(date: Date | undefined): TimeParts {
  return date ? [format(date, "HH"), format(date, "mm"), format(date, "ss")] : ["23", "59", "59"]
}

function normalizedTimeParts(parts: TimeParts): [number, number, number] {
  return [
    clampTimePart(parts[0], 23),
    clampTimePart(parts[1], 59),
    clampTimePart(parts[2], 59),
  ]
}

function clampTimePart(value: string, maximum: number): number {
  const parsed = Number.parseInt(value, 10)
  return Number.isNaN(parsed) ? 0 : Math.min(Math.max(parsed, 0), maximum)
}

function parseLocalDateTime(value: string): Date | undefined {
  if (!value) {
    return undefined
  }
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? undefined : date
}
