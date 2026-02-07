let apiKey = '';
let currentConfig = {};
const byId = (id) => document.getElementById(id);
const NUMERIC_FIELDS = new Set([
  'timeout',
  'max_retry',
  'retry_backoff_base',
  'retry_backoff_factor',
  'retry_backoff_max',
  'retry_budget',
  'refresh_interval_hours',
  'super_refresh_interval_hours',
  'fail_threshold',
  'limit_mb',
  'save_delay_ms',
  'assets_max_concurrent',
  'media_max_concurrent',
  'usage_max_concurrent',
  'assets_delete_batch_size',
  'assets_batch_size',
  'assets_max_tokens',
  'usage_batch_size',
  'usage_max_tokens',
  'reload_interval_sec',
  'stream_idle_timeout',
  'video_idle_timeout',
  'image_ws_blocked_seconds',
  'image_ws_final_min_bytes',
  'image_ws_medium_min_bytes',
  'nsfw_max_concurrent',
  'nsfw_batch_size',
  'nsfw_max_tokens'
]);

const LOCALE_MAP = {
  "app": {
    "label": "应用设置",
    "api_key": { title: "API 密钥", desc: "调用 Grok2API 服务的 Token（可选）。" },
    "app_key": { title: "后台密码", desc: "登录 Grok2API 管理后台的密码（必填）。" },
    "app_url": { title: "应用地址", desc: "当前 Grok2API 服务的外部访问 URL，用于文件链接访问。" },
    "image_format": { title: "图片格式", desc: "生成的图片格式（url 或 base64）。" },
    "video_format": { title: "视频格式", desc: "生成的视频格式（html 或 url，url 为处理后的链接）。" }
  },
  "network": {
    "label": "网络配置",
    "timeout": { title: "请求超时", desc: "请求 Grok 服务的超时时间（秒）。" },
    "base_proxy_url": { title: "基础代理 URL", desc: "代理请求到 Grok 官网的基础服务地址。" },
    "asset_proxy_url": { title: "资源代理 URL", desc: "代理请求到 Grok 官网的静态资源（图片/视频）地址。" }
  },
  "security": {
    "label": "反爬虫验证",
    "cf_clearance": { title: "CF Clearance", desc: "Cloudflare Clearance Cookie，用于绕过反爬虫验证。" },
    "browser": { title: "浏览器指纹", desc: "curl_cffi 浏览器指纹标识（如 chrome136）。" },
    "user_agent": { title: "User-Agent", desc: "HTTP 请求的 User-Agent 字符串，需与浏览器指纹匹配。" }
  },
  "chat": {
    "label": "对话配置",
    "temporary": { title: "临时对话", desc: "是否启用临时对话模式。" },
    "disable_memory": { title: "禁用记忆", desc: "禁用 Grok 记忆功能，以防止响应中出现不相关上下文。" },
    "stream": { title: "流式响应", desc: "是否默认启用流式输出。" },
    "thinking": { title: "思维链", desc: "是否启用模型思维链输出。" },
    "dynamic_statsig": { title: "动态指纹", desc: "是否启用动态生成 Statsig 值。" },
    "filter_tags": { title: "过滤标签", desc: "自动过滤 Grok 响应中的特殊标签。" }
  },
  "retry": {
    "label": "重试策略",
    "max_retry": { title: "最大重试次数", desc: "请求 Grok 服务失败时的最大重试次数。" },
    "retry_status_codes": { title: "重试状态码", desc: "触发重试的 HTTP 状态码列表。" },
    "retry_backoff_base": { title: "退避基数", desc: "重试退避的基础延迟（秒）。" },
    "retry_backoff_factor": { title: "退避倍率", desc: "重试退避的指数放大系数。" },
    "retry_backoff_max": { title: "退避上限", desc: "单次重试等待的最大延迟（秒）。" },
    "retry_budget": { title: "退避预算", desc: "单次请求的最大重试总耗时（秒）。" }
  },
  "timeout": {
    "label": "超时配置",
    "stream_idle_timeout": { title: "流空闲超时", desc: "流式响应空闲超时（秒），超过将断开。" },
    "video_idle_timeout": { title: "视频空闲超时", desc: "视频生成空闲超时（秒），超过将断开。" }
  },
  "image": {
    "label": "图片生成",
    "image_ws": { title: "WebSocket 生成", desc: "启用后 /v1/images/generations 走 WebSocket 直连。" },
    "image_ws_nsfw": { title: "NSFW 模式", desc: "WebSocket 请求是否启用 NSFW。" },
    "image_ws_blocked_seconds": { title: "Blocked 阈值", desc: "收到中等图后超过该秒数仍无最终图则判定 blocked。" },
    "image_ws_final_min_bytes": { title: "最终图最小字节", desc: "判定最终图的最小字节数（通常 JPG > 100KB）。" },
    "image_ws_medium_min_bytes": { title: "中等图最小字节", desc: "判定中等质量图的最小字节数。" }
  },
  "token": {
    "label": "Token 池管理",
    "auto_refresh": { title: "自动刷新", desc: "是否开启 Token 自动刷新机制。" },
    "refresh_interval_hours": { title: "刷新间隔", desc: "普通 Token 刷新的时间间隔（小时）。" },
    "super_refresh_interval_hours": { title: "Super 刷新间隔", desc: "Super Token 刷新的时间间隔（小时）。" },
    "fail_threshold": { title: "失败阈值", desc: "单个 Token 连续失败多少次后被标记为不可用。" },
    "save_delay_ms": { title: "保存延迟", desc: "Token 变更合并写入的延迟（毫秒）。" },
    "reload_interval_sec": { title: "同步间隔", desc: "多 worker 场景下 Token 状态刷新间隔（秒）。" }
  },
  "cache": {
    "label": "缓存管理",
    "enable_auto_clean": { title: "自动清理", desc: "是否启用缓存自动清理，开启后按上限自动回收。" },
    "limit_mb": { title: "清理阈值", desc: "缓存大小阈值（MB），超过阈值会触发清理。" }
  },
  "performance": {
    "label": "并发性能",
    "media_max_concurrent": { title: "Media 并发上限", desc: "视频/媒体生成请求的并发上限。推荐 50。" },
    "nsfw_max_concurrent": { title: "NSFW 开启并发上限", desc: "批量开启 NSFW 模式时的并发请求上限。推荐 10。" },
    "nsfw_batch_size": { title: "NSFW 开启批量大小", desc: "批量开启 NSFW 模式的单批处理数量。推荐 50。" },
    "nsfw_max_tokens": { title: "NSFW 开启最大数量", desc: "单次批量开启 NSFW 的 Token 数量上限，防止误操作。推荐 1000。" },
    "usage_max_concurrent": { title: "Token 刷新并发上限", desc: "批量刷新 Token 用量时的并发请求上限。推荐 25。" },
    "usage_batch_size": { title: "Token 刷新批次大小", desc: "批量刷新 Token 用量的单批处理数量。推荐 50。" },
    "usage_max_tokens": { title: "Token 刷新最大数量", desc: "单次批量刷新 Token 用量时的处理数量上限。推荐 1000。" },
    "assets_max_concurrent": { title: "Assets 处理并发上限", desc: "批量查找/删除资产时的并发请求上限。推荐 25。" },
    "assets_batch_size": { title: "Assets 处理批次大小", desc: "批量查找/删除资产时的单批处理数量。推荐 10。" },
    "assets_max_tokens": { title: "Assets 处理最大数量", desc: "单次批量查找/删除资产时的处理数量上限。推荐 1000。" },
    "assets_delete_batch_size": { title: "Assets 单账号删除批量大小", desc: "单账号批量删除资产时的单批并发数量。推荐 10。" }
  }
};

// 配置部分说明（可选）
const SECTION_DESCRIPTIONS = {
  "security": "配置不正确将导致 403 错误。服务首次请求 Grok 时的 IP 必须与获取 CF Clearance 时的 IP 一致，后续服务器请求 IP 变化不会导致 403。"
};

const SECTION_ORDER = new Map(Object.keys(LOCALE_MAP).map((key, index) => [key, index]));

function getText(section, key) {
  if (LOCALE_MAP[section] && LOCALE_MAP[section][key]) {
    return LOCALE_MAP[section][key];
  }
  return {
    title: key.replace(/_/g, ' '),
    desc: '暂无说明，请参考配置文档。'
  };
}

function getSectionLabel(section) {
  return (LOCALE_MAP[section] && LOCALE_MAP[section].label) || `${section} 设置`;
}

function sortByOrder(keys, orderMap) {
  if (!orderMap) return keys;
  return keys.sort((a, b) => {
    const ia = orderMap.get(a);
    const ib = orderMap.get(b);
    if (ia !== undefined && ib !== undefined) return ia - ib;
    if (ia !== undefined) return -1;
    if (ib !== undefined) return 1;
    return 0;
  });
}

function setInputMeta(input, section, key) {
  input.dataset.section = section;
  input.dataset.key = key;
}

function createOption(value, text, selectedValue) {
  const option = document.createElement('option');
  option.value = value;
  option.text = text;
  if (selectedValue !== undefined && selectedValue === value) option.selected = true;
  return option;
}

function buildBooleanInput(section, key, val) {
  const label = document.createElement('label');
  label.className = 'relative inline-flex items-center cursor-pointer';

  const input = document.createElement('input');
  input.type = 'checkbox';
  input.checked = val;
  input.className = 'sr-only peer';
  setInputMeta(input, section, key);

  const slider = document.createElement('div');
  slider.className = "w-9 h-5 bg-[var(--accents-2)] peer-focus:outline-none rounded-full peer peer-checked:after:translate-x-full peer-checked:after:border-white after:content-[''] after:absolute after:top-[2px] after:left-[2px] after:bg-white after:border-gray-300 after:border after:rounded-full after:h-4 after:w-4 after:transition-all peer-checked:bg-black";

  label.appendChild(input);
  label.appendChild(slider);

  return { input, node: label };
}

function buildSelectInput(section, key, val, options) {
  const input = document.createElement('select');
  input.className = 'geist-input h-[34px]';
  setInputMeta(input, section, key);
  options.forEach(opt => {
    input.appendChild(createOption(opt.val, opt.text, val));
  });
  return { input, node: input };
}

function buildJsonInput(section, key, val) {
  const input = document.createElement('textarea');
  input.className = 'geist-input font-mono text-xs';
  input.rows = 4;
  input.value = JSON.stringify(val, null, 2);
  setInputMeta(input, section, key);
  input.dataset.type = 'json';
  return { input, node: input };
}

function buildTextInput(section, key, val) {
  const input = document.createElement('input');
  input.type = 'text';
  input.className = 'geist-input';
  input.value = val;
  setInputMeta(input, section, key);
  return { input, node: input };
}

function buildSecretInput(section, key, val) {
  const input = document.createElement('input');
  input.type = 'text';
  input.className = 'geist-input flex-1 h-[34px]';
  input.value = val;
  setInputMeta(input, section, key);

  const wrapper = document.createElement('div');
  wrapper.className = 'flex items-center gap-2';

  const copyBtn = document.createElement('button');
  copyBtn.className = 'flex-none w-[32px] h-[32px] flex items-center justify-center bg-black text-white rounded-md hover:opacity-80 transition-opacity';
  copyBtn.type = 'button';
  copyBtn.innerHTML = `<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="13" height="13" rx="2" ry="2"></rect><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"></path></svg>`;
  copyBtn.onclick = () => copyToClipboard(input.value, copyBtn);

  wrapper.appendChild(input);
  wrapper.appendChild(copyBtn);

  return { input, node: wrapper };
}

async function init() {
  apiKey = await ensureApiKey();
  if (apiKey === null) return;
  loadData();
}

async function loadData() {
  try {
    const res = await fetch('/api/v1/admin/config', {
      headers: buildAuthHeaders(apiKey)
    });
    if (res.ok) {
      currentConfig = await res.json();
      renderConfig(currentConfig);
    } else if (res.status === 401) {
      logout();
    }
  } catch (e) {
    showToast('连接失败', 'error');
  }
}

function renderConfig(data) {
  const container = byId('config-container');
  if (!container) return;
  container.replaceChildren();

  const fragment = document.createDocumentFragment();
  const sections = sortByOrder(Object.keys(data), SECTION_ORDER);

  sections.forEach(section => {
    const items = data[section];
    const localeSection = LOCALE_MAP[section];
    const keyOrder = localeSection ? new Map(Object.keys(localeSection).map((k, i) => [k, i])) : null;

    const allKeys = sortByOrder(Object.keys(items), keyOrder);

    if (allKeys.length > 0) {
      const card = document.createElement('div');
      card.className = 'config-section';

      const header = document.createElement('div');
      header.innerHTML = `<div class="config-section-title">${getSectionLabel(section)}</div>`;
      
      // 添加部分说明（如果有）
      if (SECTION_DESCRIPTIONS[section]) {
        const descP = document.createElement('p');
        descP.className = 'text-[var(--accents-4)] text-sm mt-1 mb-4';
        descP.textContent = SECTION_DESCRIPTIONS[section];
        header.appendChild(descP);
      }
      
      card.appendChild(header);

      const grid = document.createElement('div');
      grid.className = 'config-grid';

      allKeys.forEach(key => {
        const fieldCard = buildFieldCard(section, key, items[key]);
        grid.appendChild(fieldCard);
      });

      card.appendChild(grid);
      if (grid.children.length > 0) {
        fragment.appendChild(card);
      }
    }
  });

  container.appendChild(fragment);
}

function buildFieldCard(section, key, val) {
  const text = getText(section, key);

  const fieldCard = document.createElement('div');
  fieldCard.className = 'config-field';

  // Title
  const titleEl = document.createElement('div');
  titleEl.className = 'config-field-title';
  titleEl.textContent = text.title;
  fieldCard.appendChild(titleEl);

  // Description (Muted) - 只在有描述时显示
  if (text.desc) {
    const descEl = document.createElement('p');
    descEl.className = 'config-field-desc';
    descEl.textContent = text.desc;
    fieldCard.appendChild(descEl);
  }

  // Input Wrapper
  const inputWrapper = document.createElement('div');
  inputWrapper.className = 'config-field-input';

  // Input Logic
  let built;
  if (typeof val === 'boolean') {
    built = buildBooleanInput(section, key, val);
  }
  else if (key === 'image_format') {
    built = buildSelectInput(section, key, val, [
      { val: 'url', text: 'URL' },
      { val: 'base64', text: 'Base64' }
    ]);
  }
  else if (key === 'video_format') {
    built = buildSelectInput(section, key, val, [
      { val: 'html', text: 'HTML' },
      { val: 'url', text: 'URL' }
    ]);
  }
  else if (Array.isArray(val) || typeof val === 'object') {
    built = buildJsonInput(section, key, val);
  }
  else {
    if (key === 'api_key' || key === 'app_key') {
      built = buildSecretInput(section, key, val);
    } else {
      built = buildTextInput(section, key, val);
    }
  }

  if (built) {
    inputWrapper.appendChild(built.node);
  }
  fieldCard.appendChild(inputWrapper);

  return fieldCard;
}

async function saveConfig() {
  const btn = byId('save-btn');
  const originalText = btn.innerText;
  btn.disabled = true;
  btn.innerText = '保存中...';

  try {
    const newConfig = typeof structuredClone === 'function'
      ? structuredClone(currentConfig)
      : JSON.parse(JSON.stringify(currentConfig));
    const inputs = document.querySelectorAll('input[data-section], textarea[data-section], select[data-section]');

    inputs.forEach(input => {
      const s = input.dataset.section;
      const k = input.dataset.key;
      let val = input.value;

      if (input.type === 'checkbox') {
        val = input.checked;
      } else if (input.dataset.type === 'json') {
        try { val = JSON.parse(val); } catch (e) { throw new Error(`无效的 JSON: ${getText(s, k).title}`); }
      } else if (k === 'app_key' && val.trim() === '') {
        throw new Error('app_key 不能为空（后台密码）');
      } else if (NUMERIC_FIELDS.has(k)) {
        if (val.trim() !== '' && !Number.isNaN(Number(val))) {
          val = Number(val);
        }
      }

      if (!newConfig[s]) newConfig[s] = {};
      newConfig[s][k] = val;
    });

    const res = await fetch('/api/v1/admin/config', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        ...buildAuthHeaders(apiKey)
      },
      body: JSON.stringify(newConfig)
    });

    if (res.ok) {
      btn.innerText = '成功';
      showToast('配置已保存', 'success');
      setTimeout(() => {
        btn.innerText = originalText;
        btn.style.backgroundColor = '';
      }, 2000);
    } else {
      showToast('保存失败', 'error');
    }
  } catch (e) {
    showToast('错误: ' + e.message, 'error');
  } finally {
    if (btn.innerText === '保存中...') {
      btn.disabled = false;
      btn.innerText = originalText;
    } else {
      btn.disabled = false;
    }
  }
}

async function copyToClipboard(text, btn) {
  if (!text) return;
  try {
    await navigator.clipboard.writeText(text);

    btn.innerHTML = `<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="20 6 9 17 4 12"></polyline></svg>`;
    btn.style.backgroundColor = '#10b981';
    btn.style.borderColor = '#10b981';

    setTimeout(() => {
      btn.innerHTML = `<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="13" height="13" rx="2" ry="2"></rect><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"></path></svg>`;
      btn.style.backgroundColor = '';
      btn.style.borderColor = '';
    }, 2000);
  } catch (err) {
    console.error('Failed to copy', err);
  }
}

window.onload = init;
