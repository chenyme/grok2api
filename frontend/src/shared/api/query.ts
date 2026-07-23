import type { SortOrder } from "@/shared/lib/table-sort";

export type PaginatedQueryInput = {
  page: number;
  pageSize: number;
  search?: string;
  sortBy?: string;
  sortOrder?: SortOrder;
};

/**
 * 构建分页查询参数。自动处理 page/pageSize/search/sortBy/sortOrder，
 * 额外的过滤字段可通过 extra 参数传入。
 */
export function buildPaginatedQuery(input: PaginatedQueryInput, extra?: Record<string, string | undefined>): URLSearchParams {
  const query = new URLSearchParams({ page: String(input.page), pageSize: String(input.pageSize) });
  if (input.search) query.set("search", input.search);
  if (input.sortBy && input.sortOrder) {
    query.set("sortBy", input.sortBy);
    query.set("sortOrder", input.sortOrder);
  }
  if (extra) {
    for (const [key, value] of Object.entries(extra)) {
      if (value) query.set(key, value);
    }
  }
  return query;
}
