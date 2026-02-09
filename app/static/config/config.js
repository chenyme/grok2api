(() => {
  const IS_SPA = window.__GROK_ADMIN_SPA__ === true;

  let apiKey = "";
  let currentConfig = {};
  let configInitialized = false;
  const byId = (id) => document.getElementById(id);

  const NUMERIC_FIELDS = new Set([
    "timeout",
    "max_retry",
    "retry_backoff_base",
    "retry_backoff_factor",
    "retry_backoff_max",
    "retry_budget",
    "refresh_interval_hours",
    "super_refresh_interval_hours",
    "fail_threshold",
    "image_limit_mb",
    "video_limit_mb",
    "save_delay_ms",
    "assets_max_concurrent",
    "media_max_concurrent",
    "usage_max_concurrent",
    "assets_delete_batch_size",
    "assets_batch_size",
    "assets_max_tokens",
    "usage_batch_size",
    "usage_max_tokens",
    "reload_interval_sec",
    "stream_idle_timeout",
    "video_idle_timeout",
    "nsfw_max_concurrent",
    "nsfw_batch_size",
    "nsfw_max_tokens",
  ]);

  const GROK_COMPAT_SECTION_MAP = Object.freeze({
    temporary: "chat",
    stream: "chat",
    thinking: "chat",
    dynamic_statsig: "chat",
    cf_clearance: "security",
    base_proxy_url: "network",
    asset_proxy_url: "network",
    timeout: "network",
    stream_idle_timeout: "timeout",
    video_idle_timeout: "timeout",
    max_retry: "retry",
    retry_backoff_base: "retry",
    retry_backoff_factor: "retry",
    retry_backoff_max: "retry",
    retry_budget: "retry",
  });

  // T佬风格：配置分组映射
  const CONFIG_GROUPS = {
    global: {
      title: "全局配置",
      cards: [
        {
          title: "系统设置",
          fields: [
            {
              section: "app",
              key: "app_username",
              label: "登陆账户",
              highlight: true,
              tip: "后台登录账号",
            },
            {
              section: "app",
              key: "app_password",
              label: "登陆密码",
              highlight: true,
              secret: true,
              tip: "后台登录密码",
            },
            {
              section: "app",
              key: "api_key",
              label: "API 密钥",
              highlight: true,
              secret: true,
              placeholder: "对外接口密钥 (留空则无需验证)",
              tip: "对外 API 访问密钥，留空则不校验",
            },
          ],
        },
        {
          title: "媒体设置",
          fields: [
            {
              section: "app",
              key: "image_format",
              label: "图片模式",
              type: "select",
              tip: "图片返回格式：URL 或 Base64",
              options: [
                { val: "url", text: "URL链接" },
                { val: "base64", text: "Base64" },
              ],
            },
            {
              section: "app",
              key: "app_url",
              label: "服务网址",
              tip: "服务对外访问地址",
            },
            {
              section: "app",
              key: "video_format",
              label: "视频模式",
              type: "select",
              tip: "视频返回格式：Markdown 或 URL",
              options: [
                { val: "html", text: "Markdown" },
                { val: "url", text: "URL" },
              ],
            },
          ],
        },
        {
          title: "缓存管理",
          fields: [
            {
              section: "cache",
              key: "enable_auto_clean",
              label: "自动清理",
              type: "toggle",
              tip: "开启后自动清理缓存",
            },
            {
              section: "cache",
              key: "image_limit_mb",
              label: "图片缓存 (MB)",
              type: "number",
              tip: "图片缓存上限，单位 MB",
            },
            {
              section: "cache",
              key: "video_limit_mb",
              label: "视频缓存 (MB)",
              type: "number",
              tip: "视频缓存上限，单位 MB",
            },
          ],
        },
      ],
    },
    grok: {
      title: "Grok 配置",
      cards: [
        {
          title: "基础设置",
          fields: [
            {
              section: "chat",
              key: "temporary",
              label: "临时对话",
              type: "toggle",
              tip: "开启后默认使用临时对话",
            },
            {
              section: "chat",
              key: "stream",
              label: "流式响应",
              type: "toggle",
              tip: "开启后返回流式响应",
            },
            {
              section: "chat",
              key: "thinking",
              label: "思维链",
              type: "toggle",
              tip: "开启后返回思维链",
            },
            {
              section: "chat",
              key: "dynamic_statsig",
              label: "动态指纹",
              type: "toggle",
              tip: "开启后使用动态指纹",
            },
          ],
        },
        {
          title: "代理设置",
          fields: [
            {
              section: "security",
              key: "cf_clearance",
              label: "CF Clearance",
              secret: true,
              tip: "Cloudflare Clearance Cookie",
            },
            {
              section: "network",
              key: "base_proxy_url",
              label: "Proxy Url (服务代理)",
              placeholder: "socks5://username:password@127.0.0.1:7890",
              tip: "主请求代理地址",
            },
            {
              section: "network",
              key: "asset_proxy_url",
              label: "Asset Proxy Url (资源代理)",
              placeholder: "socks5://username:password@127.0.0.1:7890",
              tip: "资源/媒体请求代理地址",
            },
          ],
        },
        {
          title: "超时设置",
          fields: [
            {
              section: "network",
              key: "timeout",
              label: "请求超时 (秒)",
              type: "number",
              tip: "请求总超时，单位秒",
            },
            {
              section: "timeout",
              key: "stream_idle_timeout",
              label: "流式间隔超时 (秒)",
              type: "number",
              tip: "流式响应空闲超时，单位秒",
            },
            {
              section: "timeout",
              key: "video_idle_timeout",
              label: "视频生成超时 (秒)",
              type: "number",
              tip: "视频生成超时，单位秒",
            },
          ],
        },
        {
          title: "重试设置",
          horizontal: true,
          fields: [
            {
              section: "retry",
              key: "max_retry",
              label: "最大重试次数",
              type: "number",
              tip: "失败后最大重试次数",
            },
            {
              section: "retry",
              key: "retry_backoff_base",
              label: "退避基数 (秒)",
              type: "number",
              tip: "退避基数（秒）",
            },
            {
              section: "retry",
              key: "retry_backoff_factor",
              label: "退避倍率",
              type: "number",
              tip: "退避倍率",
            },
            {
              section: "retry",
              key: "retry_backoff_max",
              label: "退避上限 (秒)",
              type: "number",
              tip: "退避上限（秒）",
            },
            {
              section: "retry",
              key: "retry_budget",
              label: "退避预算 (秒)",
              type: "number",
              tip: "重试预算总时长（秒）",
            },
          ],
        },
      ],
    },
    token: {
      title: "Token 池配置",
      cards: [
        {
          title: "Token 设置",
          horizontal: true,
          fields: [
            {
              section: "token",
              key: "auto_refresh",
              label: "自动刷新",
              type: "toggle",
              tip: "自动刷新 Token",
            },
            {
              section: "token",
              key: "refresh_interval_hours",
              label: "刷新间隔 (小时)",
              type: "number",
              tip: "常规刷新间隔（小时）",
            },
            {
              section: "token",
              key: "super_refresh_interval_hours",
              label: "Super 刷新间隔 (小时)",
              type: "number",
              tip: "强制刷新间隔（小时）",
            },
            {
              section: "token",
              key: "fail_threshold",
              label: "失败阈值",
              type: "number",
              tip: "失败次数达到阈值将刷新",
            },
            {
              section: "token",
              key: "save_delay_ms",
              label: "保存延迟 (ms)",
              type: "number",
              tip: "保存延迟，单位毫秒",
            },
            {
              section: "token",
              key: "reload_interval_sec",
              label: "一致性刷新 (秒)",
              type: "number",
              tip: "一致性刷新间隔（秒）",
            },
          ],
        },
      ],
    },
    performance: {
      title: "性能配置",
      cards: [
        {
          title: "媒体并发",
          fields: [
            {
              section: "performance",
              key: "media_max_concurrent",
              label: "Media 并发上限",
              type: "number",
              tip: "媒体处理并发上限",
            },
          ],
        },
        {
          title: "NSFW 批量",
          fields: [
            {
              section: "performance",
              key: "nsfw_max_concurrent",
              label: "并发上限",
              type: "number",
              tip: "NSFW 检测并发上限",
            },
            {
              section: "performance",
              key: "nsfw_batch_size",
              label: "批量大小",
              type: "number",
              tip: "NSFW 单批处理数量",
            },
            {
              section: "performance",
              key: "nsfw_max_tokens",
              label: "最大数量",
              type: "number",
              tip: "NSFW 单批最大 token 数",
            },
          ],
        },
        {
          title: "Token 刷新",
          fields: [
            {
              section: "performance",
              key: "usage_max_concurrent",
              label: "并发上限",
              type: "number",
              tip: "Token 刷新并发上限",
            },
            {
              section: "performance",
              key: "usage_batch_size",
              label: "批量大小",
              type: "number",
              tip: "Token 刷新批量大小",
            },
            {
              section: "performance",
              key: "usage_max_tokens",
              label: "最大数量",
              type: "number",
              tip: "Token 刷新单批最大 token 数",
            },
          ],
        },
      ],
    },
  };

  function helpIcon(tip) {
    const safeTip = escapeHtml(tip || "");
    return `<span class="help-icon tooltip" data-tip="${safeTip}"><svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><path d="M9.09 9a3 3 0 0 1 5.83 1c0 2-3 3-3 3"/><line x1="12" y1="17" x2="12.01" y2="17"/></svg></span>`;
  }

  function createToggle(section, key, checked) {
    const id = `toggle-${section}-${key}`;
    return `<div class="toggle-switch ${checked ? "active" : ""}" id="${id}" data-section="${section}" data-key="${key}" onclick="GrokAdminPages.config.actions.toggleSwitch(this)"></div>`;
  }

  function createInput(section, key, value, opts = {}) {
    const cls = opts.highlight ? "config-input highlight" : "config-input";
    const type = opts.type === "number" ? "number" : "text";
    const placeholder = opts.placeholder || "";

    if (opts.secret) {
      return `
      <div class="input-with-action">
        <input type="${type}" class="${cls}" id="input-${section}-${key}" data-section="${section}" data-key="${key}" value="${escapeHtml(value)}" placeholder="${placeholder}">
        <button type="button" class="input-action-btn" onclick="GrokAdminPages.config.actions.togglePassword(this)" title="显示/隐藏">
          <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"/><circle cx="12" cy="12" r="3"/></svg>
        </button>
      </div>`;
    }

    return `<input type="${type}" class="${cls}" id="input-${section}-${key}" data-section="${section}" data-key="${key}" value="${escapeHtml(value)}" placeholder="${placeholder}">`;
  }

  function createSelect(section, key, value, options) {
    let html = `<select class="config-select" id="input-${section}-${key}" data-section="${section}" data-key="${key}">`;
    options.forEach((opt) => {
      const selected = opt.val === value ? " selected" : "";
      html += `<option value="${opt.val}"${selected}>${opt.text}</option>`;
    });
    html += "</select>";
    return html;
  }

  function escapeHtml(str) {
    if (str === null || str === undefined) return "";
    return String(str)
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;");
  }

  function toggleSwitch(el) {
    el.classList.toggle("active");
  }

  function togglePassword(btn) {
    const input = btn.parentElement.querySelector("input");
    if (input.type === "password") {
      input.type = "text";
      btn.innerHTML = `<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M17.94 17.94A10.07 10.07 0 0 1 12 20c-7 0-11-8-11-8a18.45 18.45 0 0 1 5.06-5.94M9.9 4.24A9.12 9.12 0 0 1 12 4c7 0 11 8 11 8a18.5 18.5 0 0 1-2.16 3.19m-6.72-1.07a3 3 0 1 1-4.24-4.24"/><line x1="1" y1="1" x2="23" y2="23"/></svg>`;
    } else {
      input.type = "password";
      btn.innerHTML = `<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"/><circle cx="12" cy="12" r="3"/></svg>`;
    }
  }

  function getValue(section, key) {
    const direct = currentConfig[section]?.[key];
    if (direct !== undefined && direct !== null) {
      return direct;
    }

    const legacySection = GROK_COMPAT_SECTION_MAP[key];
    if (legacySection === section) {
      const legacyValue = currentConfig.grok?.[key];
      if (legacyValue !== undefined && legacyValue !== null) {
        return legacyValue;
      }
    }

    return "";
  }

  function renderGroup(groupKey) {
    const group = CONFIG_GROUPS[groupKey];
    if (!group) return;

    const container = byId(`${groupKey}-config`);
    if (!container) return;

    let html = "";

    group.cards.forEach((card) => {
      const cardClass = card.horizontal
        ? "config-card horizontal"
        : "config-card";
      html += `<div class="${cardClass}">`;
      html += `<div class="config-card-title">${card.title}</div>`;

      // 横版卡片用 grid 容器
      if (card.horizontal) {
        html += `<div class="config-fields-grid">`;
      }

      card.fields.forEach((field) => {
        const value = getValue(field.section, field.key);
        html += `<div class="config-field">`;
        html += `<label class="config-field-label">${field.label} ${helpIcon(field.tip || field.label)}</label>`;

        if (field.type === "toggle") {
          html += createToggle(field.section, field.key, !!value);
        } else if (field.type === "select") {
          html += createSelect(field.section, field.key, value, field.options);
        } else {
          html += createInput(field.section, field.key, value, field);
        }

        html += `</div>`;
      });

      // 关闭 grid 容器
      if (card.horizontal) {
        html += `</div>`;
      }

      html += `</div>`;
    });

    container.innerHTML = html;
  }

  function renderAll() {
    Object.keys(CONFIG_GROUPS).forEach((groupKey) => {
      renderGroup(groupKey);
    });
  }

  async function init() {
    apiKey = await ensureApiKey();
    if (apiKey === null) return;
    await loadData();
  }

  async function loadData() {
    if (!configInitialized) return;
    try {
      const res = await fetch("/api/v1/admin/config", {
        headers: buildAuthHeaders(apiKey),
      });
      if (res.ok) {
        currentConfig = await res.json();
        renderAll();
      } else if (res.status === 401) {
        logout();
      }
    } catch (e) {
      showToast("连接失败", "error");
    }
  }

  async function saveConfig(groupKey, ev) {
    if (!configInitialized) return;
    const btn = ev && ev.target ? ev.target : null;
    const originalText = btn ? btn.innerText : "";
    if (btn) {
      btn.disabled = true;
      btn.innerText = "保存中...";
    }

    try {
      // 收集所有输入值
      const newConfig =
        typeof structuredClone === "function"
          ? structuredClone(currentConfig)
          : JSON.parse(JSON.stringify(currentConfig));

      // 收集 input 和 select 值
      document
        .querySelectorAll("input[data-section], select[data-section]")
        .forEach((el) => {
          const s = el.dataset.section;
          const k = el.dataset.key;
          let val = el.value;

          if (NUMERIC_FIELDS.has(k)) {
            if (val.trim() !== "" && !Number.isNaN(Number(val))) {
              val = Number(val);
            }
          }

          if (!newConfig[s]) newConfig[s] = {};
          newConfig[s][k] = val;
        });

      // 收集 toggle 值
      document
        .querySelectorAll(".toggle-switch[data-section]")
        .forEach((el) => {
          const s = el.dataset.section;
          const k = el.dataset.key;
          const val = el.classList.contains("active");

          if (!newConfig[s]) newConfig[s] = {};
          newConfig[s][k] = val;
        });

      // 清理过时 grok 分组（重启会被迁移器删除，提前清理避免混淆）
      if (newConfig.grok && typeof newConfig.grok === "object") {
        delete newConfig.grok;
      }

      // 验证
      if (!newConfig.app?.app_password?.trim()) {
        throw new Error("登陆密码不能为空");
      }

      const res = await fetch("/api/v1/admin/config", {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          ...buildAuthHeaders(apiKey),
        },
        body: JSON.stringify(newConfig),
      });

      if (res.ok) {
        currentConfig = newConfig;
        showToast("配置已保存", "success");
        if (btn) {
          btn.innerText = "已保存";
          setTimeout(() => {
            btn.innerText = originalText;
          }, 2000);
        }
      } else {
        showToast("保存失败", "error");
      }
    } catch (e) {
      showToast("错误: " + e.message, "error");
    } finally {
      if (btn) {
        btn.disabled = false;
        if (btn.innerText === "保存中...") {
          btn.innerText = originalText;
        }
      }
    }
  }

  function resetConfigState() {
    apiKey = "";
    currentConfig = {};
  }

  function cleanupConfigPage() {
    resetConfigState();
    configInitialized = false;
  }

  function initConfigPage() {
    cleanupConfigPage();
    configInitialized = true;
    init();
  }

  const configActions = {
    saveConfig,
    toggleSwitch,
    togglePassword,
  };

  function registerConfigPage() {
    window.GrokAdminPages = window.GrokAdminPages || {};
    window.GrokAdminPages.config = {
      init: initConfigPage,
      cleanup: cleanupConfigPage,
      actions: configActions,
    };
  }

  registerConfigPage();

  if (!IS_SPA) {
    window.addEventListener("load", () => {
      configInitialized = true;
      init();
    });
  }
})();
