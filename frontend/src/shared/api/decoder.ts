export type ApiDecoder<T> = (value: unknown) => T;
export type ValueValidator = (value: unknown) => boolean;

export const isString: ValueValidator = (value) => typeof value === "string";
export const isNumber: ValueValidator = (value) => typeof value === "number" && Number.isFinite(value);
export const isBoolean: ValueValidator = (value) => typeof value === "boolean";
export const isObject: ValueValidator = (value) => typeof value === "object" && value !== null && !Array.isArray(value);

export function isOptional(validator: ValueValidator): ValueValidator {
  return (value) => value === undefined || validator(value);
}

export function isArrayOf(validator: ValueValidator): ValueValidator {
  return (value) => Array.isArray(value) && value.every(validator);
}

export function isRecordOf(validator: ValueValidator): ValueValidator {
  return (value) => {
    if (!isObject(value)) return false;
    return Object.values(value as Record<string, unknown>).every(validator);
  };
}

export function isOneOf<const T extends readonly string[]>(...values: T): ValueValidator {
  return (value) => typeof value === "string" && values.includes(value);
}

export function hasShape(shape: Readonly<Record<string, ValueValidator>>): ValueValidator {
  return (value) => {
    if (!isObject(value)) return false;
    const record = value as Record<string, unknown>;
    return Object.entries(shape).every(([key, validator]) => validator(record[key]));
  };
}

export function createObjectDecoder<T>(name: string, shape: Readonly<Record<string, ValueValidator>>): ApiDecoder<T> {
  return createValidatedDecoder(name, hasShape(shape));
}

export function createValidatedDecoder<T>(name: string, validator: ValueValidator): ApiDecoder<T> {
  return (value) => {
    if (!validator(value)) throw new Error(`${name} response shape is invalid`);
    // shape 已逐字段验证；断言只保留在统一外部输入边界。
    return value as T;
  };
}

export function decodeBooleanResult<T>(field: string): ApiDecoder<T> {
  return createObjectDecoder<T>("boolean result", { [field]: isBoolean });
}

export function decodeCountResult<T>(field: string): ApiDecoder<T> {
  return createObjectDecoder<T>("count result", { [field]: isNumber });
}

export function createPaginatedDecoder<T>(itemValidator: ValueValidator): ApiDecoder<{ items: T[]; page: number; pageSize: number; total: number }> {
  return createObjectDecoder("paginated result", {
    items: isArrayOf(itemValidator),
    page: isNumber,
    pageSize: isNumber,
    total: isNumber,
  });
}
