import { useCallback, useMemo, useState } from "react";

import type { TableSort } from "@/shared/lib/table-sort";
import { nextTableSort } from "@/shared/lib/table-sort";

export type DataTableState<Field extends string = string> = {
  page: number;
  pageSize: number;
  sort: TableSort<Field>;
  selected: Set<string>;
  setPage: (page: number) => void;
  setPageSize: (size: number) => void;
  changeSort: (field: Field) => void;
  toggleItem: (id: string) => void;
  togglePage: (ids: string[], select: boolean) => void;
  clearSelection: () => void;
  selectedArray: string[];
};

/**
 * 封装数据表格页面的通用状态：分页、排序、行选择。
 * 供 accounts、client-keys、models 等列表页复用。
 */
export function useDataTable<Field extends string = string>(
  initialSort: TableSort<Field>,
  initialPageSize = 20,
): DataTableState<Field> {
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(initialPageSize);
  const [sort, setSort] = useState<TableSort<Field>>(initialSort);
  const [selected, setSelected] = useState<Set<string>>(new Set());

  const changeSort = useCallback((field: Field) => {
    setSort((current) => nextTableSort(current, field));
    setPage(1);
  }, []);

  const toggleItem = useCallback((id: string) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }, []);

  const togglePage = useCallback((ids: string[], select: boolean) => {
    setSelected((prev) => {
      const next = new Set(prev);
      for (const id of ids) {
        if (select) next.add(id);
        else next.delete(id);
      }
      return next;
    });
  }, []);

  const clearSelection = useCallback(() => setSelected(new Set()), []);

  const selectedArray = useMemo(() => [...selected], [selected]);

  return { page, pageSize, sort, selected, setPage, setPageSize, changeSort, toggleItem, togglePage, clearSelection, selectedArray };
}
