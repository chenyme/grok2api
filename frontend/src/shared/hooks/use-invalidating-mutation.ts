import { useMutation, useQueryClient, type QueryKey } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";

import { showError } from "@/shared/lib/show-error";

type InvalidatingMutationOptions<TData, TVariables> = {
  mutationFn: (variables: TVariables) => Promise<TData>;
  /** Query keys to invalidate on success. */
  invalidateKeys: QueryKey[];
  /** i18n key for the success toast. If omitted, no toast is shown. */
  successKey?: string;
  /** Additional side-effect after invalidation (e.g. close dialog, clear selection). */
  onSuccess?: (data: TData, variables: TVariables) => void;
};

/**
 * 封装 useMutation 的通用模式：调用 API → 失效缓存 → 弹出成功提示 → 错误兜底。
 * 减少各页面中重复的 onSuccess/onError 样板代码。
 */
export function useInvalidatingMutation<TData = unknown, TVariables = void>(
  options: InvalidatingMutationOptions<TData, TVariables>,
) {
  const queryClient = useQueryClient();
  const { t } = useTranslation();

  return useMutation<TData, Error, TVariables>({
    mutationFn: options.mutationFn,
    onSuccess: (data, variables) => {
      for (const key of options.invalidateKeys) {
        void queryClient.invalidateQueries({ queryKey: key });
      }
      if (options.successKey) {
        toast.success(t(options.successKey));
      }
      options.onSuccess?.(data, variables);
    },
    onError: showError,
  });
}
