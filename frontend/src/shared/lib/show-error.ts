import { toast } from "sonner";

import { i18n } from "@/shared/i18n";

/**
 * 统一的错误提示工具：从 unknown 错误中提取消息并弹出 toast。
 * 签名与 TanStack Query onError 回调兼容。
 */
export function showError(error: unknown): void {
  toast.error(error instanceof Error ? error.message : i18n.t("errors.generic"));
}
