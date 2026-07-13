export type SortOrder = "asc" | "desc";

export type TableSort<Field extends string = string> = {
  field: Field;
  order: SortOrder;
};

export function nextTableSort<Field extends string>(current: TableSort<Field>, field: Field, initialOrder: SortOrder = "asc"): TableSort<Field> {
  if (current.field !== field) {
    return { field, order: initialOrder };
  }
  return { field, order: current.order === "asc" ? "desc" : "asc" };
}
