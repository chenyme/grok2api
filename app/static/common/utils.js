/**
 * 公共工具库 - 性能优化版
 */

const Utils = {
  /**
   * 防抖 - 延迟执行，多次调用只执行最后一次
   */
  debounce(fn, delay = 300) {
    let timeoutId;
    return function (...args) {
      clearTimeout(timeoutId);
      timeoutId = setTimeout(() => fn.apply(this, args), delay);
    };
  },

  /**
   * 节流 - 限制执行频率
   */
  throttle(fn, limit = 16) {
    let lastTime = 0;
    return function (...args) {
      const now = performance.now();
      if (now - lastTime >= limit) {
        lastTime = now;
        fn.apply(this, args);
      }
    };
  },

  /**
   * requestAnimationFrame 节流
   */
  rafThrottle(fn) {
    let rafId = null;
    return function (...args) {
      if (rafId) return;
      rafId = requestAnimationFrame(() => {
        fn.apply(this, args);
        rafId = null;
      });
    };
  },

  /**
   * 批量 DOM 更新 - 使用 DocumentFragment
   */
  batchDOMUpdate(container, items, renderFn) {
    const fragment = document.createDocumentFragment();
    items.forEach((item, index) => {
      const el = renderFn(item, index);
      if (el) fragment.appendChild(el);
    });
    container.replaceChildren(fragment);
  },

  /**
   * 安全的 HTML 转义
   */
  escapeHtml(str) {
    if (!str) return "";
    const div = document.createElement("div");
    div.textContent = str;
    return div.innerHTML;
  },

  /**
   * 事件监听器管理器 - 防止内存泄漏
   */
  createEventManager() {
    const listeners = [];
    return {
      add(target, event, handler, options) {
        target.addEventListener(event, handler, options);
        listeners.push({ target, event, handler, options });
      },
      removeAll() {
        listeners.forEach(({ target, event, handler, options }) => {
          target.removeEventListener(event, handler, options);
        });
        listeners.length = 0;
      },
    };
  },

  /**
   * 请求去重器 - 防止重复请求
   */
  createRequestDeduper() {
    const pending = new Map();
    return async function dedupe(key, requestFn) {
      if (pending.has(key)) {
        return pending.get(key);
      }
      const promise = requestFn().finally(() => {
        pending.delete(key);
      });
      pending.set(key, promise);
      return promise;
    };
  },

  /**
   * 简单的 LRU 缓存
   */
  createCache(maxSize = 100) {
    const cache = new Map();
    return {
      get(key) {
        if (!cache.has(key)) return undefined;
        const value = cache.get(key);
        cache.delete(key);
        cache.set(key, value);
        return value;
      },
      set(key, value) {
        if (cache.has(key)) cache.delete(key);
        else if (cache.size >= maxSize) {
          const firstKey = cache.keys().next().value;
          cache.delete(firstKey);
        }
        cache.set(key, value);
      },
      clear() {
        cache.clear();
      },
    };
  },

  /**
   * DOM 元素缓存
   */
  createElementCache() {
    const cache = new Map();
    return {
      get(id) {
        if (!cache.has(id)) {
          cache.set(id, document.getElementById(id));
        }
        return cache.get(id);
      },
      clear() {
        cache.clear();
      },
    };
  },
};

// 全局请求去重器
const requestDeduper = Utils.createRequestDeduper();

// 全局事件管理器（页面卸载时自动清理）
const globalEventManager = Utils.createEventManager();
window.addEventListener("beforeunload", () => {
  globalEventManager.removeAll();
});
