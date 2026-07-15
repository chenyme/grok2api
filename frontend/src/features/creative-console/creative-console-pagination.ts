export type PaginatedPage<T> = {
  items: T[];
  page: number;
  pageSize: number;
  total: number;
};

type PaginationOptions = {
  pageSize?: number;
  maxPages?: number;
};

const maxPageSize = 100;
const defaultMaxPages = 50;

export async function listAllPaginatedItems<T>(
  loadPage: (page: number, pageSize: number) => Promise<PaginatedPage<T>>,
  options: PaginationOptions = {},
): Promise<T[]> {
  const pageSize = boundedInteger(options.pageSize ?? maxPageSize, 1, maxPageSize);
  const maxPages = boundedInteger(options.maxPages ?? defaultMaxPages, 1, defaultMaxPages);
  const items: T[] = [];
  let expectedTotal = Number.POSITIVE_INFINITY;

  for (let page = 1; page <= maxPages; page += 1) {
    const result = await loadPage(page, pageSize);
    expectedTotal = normalizeTotal(result.total, expectedTotal);
    items.push(...result.items);

    if (items.length >= expectedTotal) return items.slice(0, expectedTotal);
    if (result.items.length < pageSize) return items;
  }

  return Number.isFinite(expectedTotal) ? items.slice(0, expectedTotal) : items;
}

function boundedInteger(value: number, min: number, max: number): number {
  if (!Number.isFinite(value)) return max;
  return Math.min(max, Math.max(min, Math.floor(value)));
}

function normalizeTotal(value: number, fallback: number): number {
  if (!Number.isFinite(value)) return fallback;
  return Math.max(0, Math.floor(value));
}
