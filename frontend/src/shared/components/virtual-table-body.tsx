import { useCallback, useLayoutEffect, useRef, useState, type ReactNode } from "react";

import { TableBody, TableCell, TableRow } from "@/components/ui/table";

const DEFAULT_OVERSCAN = 12;
const MIN_VIRTUALIZED_ROWS = 20;

type VirtualTableBodyProps<T> = {
  items: readonly T[];
  colSpan: number;
  rowHeight: number;
  renderRow: (item: T, index: number) => ReactNode;
  overscan?: number;
};

type VisibleRange = { start: number; end: number };

// VirtualTableBody keeps the native table structure and an internal table
// viewport while mounting only the visible rows. Fixed-height management tables
// are a good fit and keep large page sizes from creating thousands of Radix
// controls, tooltips, and menu triggers at once.
export function VirtualTableBody<T>({ items, colSpan, rowHeight, renderRow, overscan = DEFAULT_OVERSCAN }: VirtualTableBodyProps<T>) {
  const bodyRef = useRef<HTMLTableSectionElement>(null);
  const frameRef = useRef<number | null>(null);
  const enabled = items.length > MIN_VIRTUALIZED_ROWS;
  const [range, setRange] = useState<VisibleRange>(() => ({ start: 0, end: enabled ? Math.min(items.length, 50) : items.length }));

  const updateRange = useCallback(() => {
    if (!enabled || !bodyRef.current) {
      setRange((current) => current.start === 0 && current.end === items.length ? current : { start: 0, end: items.length });
      return;
    }
    const rect = bodyRef.current.getBoundingClientRect();
    const scrollContainer = bodyRef.current.closest<HTMLElement>("[data-slot=table-scroll-container]");
    const containerRect = scrollContainer?.getBoundingClientRect();
    const viewportTop = Math.max(0, containerRect?.top ?? 0);
    const viewportBottom = Math.min(window.innerHeight, containerRect?.bottom ?? window.innerHeight);
    const relativeTop = Math.max(0, viewportTop - rect.top);
    const relativeBottom = Math.min(rect.height, viewportBottom - rect.top);
    const start = Math.max(0, Math.floor(relativeTop / rowHeight) - overscan);
    const end = Math.min(items.length, Math.ceil(Math.max(relativeTop, relativeBottom) / rowHeight) + overscan);
    setRange((current) => current.start === start && current.end === end ? current : { start, end });
  }, [enabled, items.length, overscan, rowHeight]);

  useLayoutEffect(() => {
    const scrollContainer = bodyRef.current?.closest<HTMLElement>("[data-slot=table-scroll-container]") ?? null;
    const scheduleUpdate = () => {
      if (frameRef.current !== null) return;
      frameRef.current = window.requestAnimationFrame(() => {
        frameRef.current = null;
        updateRange();
      });
    };
    scheduleUpdate();
    window.addEventListener("scroll", scheduleUpdate, { passive: true });
    window.addEventListener("resize", scheduleUpdate);
    scrollContainer?.addEventListener("scroll", scheduleUpdate, { passive: true });
    let observer: ResizeObserver | null = null;
    if (typeof ResizeObserver !== "undefined" && scrollContainer !== null) {
      observer = new ResizeObserver(scheduleUpdate);
      observer.observe(scrollContainer);
    }
    return () => {
      window.removeEventListener("scroll", scheduleUpdate);
      window.removeEventListener("resize", scheduleUpdate);
      scrollContainer?.removeEventListener("scroll", scheduleUpdate);
      observer?.disconnect();
      if (frameRef.current !== null) window.cancelAnimationFrame(frameRef.current);
      frameRef.current = null;
    };
  }, [updateRange]);

  const safeStart = enabled && items.length > 0 ? Math.min(range.start, items.length - 1) : 0;
  const safeEnd = enabled && items.length > 0 ? Math.max(safeStart + 1, Math.min(items.length, range.end)) : 0;
  const visibleItems = enabled ? items.slice(safeStart, safeEnd) : items;
  const topHeight = enabled ? safeStart * rowHeight : 0;
  const bottomHeight = enabled ? Math.max(0, items.length - safeEnd) * rowHeight : 0;

  return (
    <TableBody ref={bodyRef}>
      {topHeight > 0 ? <VirtualSpacer colSpan={colSpan} height={topHeight} /> : null}
      {visibleItems.map((item, index) => renderRow(item, safeStart + index))}
      {bottomHeight > 0 ? <VirtualSpacer colSpan={colSpan} height={bottomHeight} /> : null}
    </TableBody>
  );
}

function VirtualSpacer({ colSpan, height }: { colSpan: number; height: number }) {
  return (
    <TableRow aria-hidden="true" className="pointer-events-none border-0 hover:bg-transparent">
      <TableCell colSpan={colSpan} className="p-0" style={{ height }} />
    </TableRow>
  );
}
