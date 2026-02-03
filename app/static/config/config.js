let apiKey = '';
let currentConfig = {};
const byId = (id) => document.getElementById(id);
const NUMERIC_FIELDS = new Set([
  'timeout',
  'max_retry',
  'refresh_interval_hours',
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
  'nsfw_max_concurrent',
  'nsfw_batch_size',
  'nsfw_max_tokens'
]);

const LOCALE_MAP = {
  "app": {
    "label": "应用设置",
    "api_key": { title: "API 密钥", desc: "调用 Grok2API 服务所需的 Bearer Token，请妥善保管。" },
    "app_key": { title: "后台密码", desc: "登录 Grok2API 服务管理后台的密码，请妥善保管。" },
    "app_url": { title: "应用地址", desc: "当前 Grok2API 服务的外部访问 URL，用于文件链接访问。" },
    "image_format": { title: "图片格式", desc: "生成的图片格式（url 或 base64）。" },
    "video_format": { title: "视频格式", desc: "生成的视频格式（仅支持 url）。" }
  },
  "grok": {
    "label": "Grok 设置",
    "temporary": { title: "临时对话", desc: "是否启用临时对话模式。" },
    "stream": { title: "流式响应", desc: "是否默认启用流式输出。" },
    "thinking": { title: "思维链", desc: "是否启用模型思维链输出。" },
    "dynamic_statsig": { title: "动态指纹", desc: "是否启用动态生成 Statsig 值。" },
    "filter_tags": { title: "过滤标签", desc: "自动过滤 Grok 响应中的特殊标签。" },
    "timeout": { title: "超时时间", desc: "请求 Grok 服务的超时时间（秒）。" },
    "base_proxy_url": { title: "基础代理 URL", desc: "代理请求到 Grok 官网的基础服务地址。" },
    "asset_proxy_url": { title: "资源代理 URL", desc: "代理请求到 Grok 官网的静态资源（图片/视频）地址。" },
    "cf_clearance": { title: "CF Clearance", desc: "Cloudflare 验证 Cookie，用于验证 Cloudflare 的验证。" },
    "max_retry": { title: "最大重试", desc: "请求 Grok 服务失败时的最大重试次数。" },
    "retry_status_codes": { title: "重试状态码", desc: "触发重试的 HTTP 状态码列表。" }
  },
  "token": {
    "label": "Token 池设置",
    "auto_refresh": { title: "自动刷新", desc: "是否开启 Token 自动刷新机制。" },
    "refresh_interval_hours": { title: "刷新间隔", desc: "Token 刷新的时间间隔（小时）。" },
    "fail_threshold": { title: "失败阈值", desc: "单个 Token 连续失败多少次后被标记为不可用。" },
    "save_delay_ms": { title: "保存延迟", desc: "Token 变更合并写入的延迟（毫秒）。" },
    "reload_interval_sec": { title: "一致性刷新", desc: "多 worker 场景下 Token 状态刷新间隔（秒）。" }
  },
  "cache": {
    "label": "缓存设置",
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

    const card = document.createElement('div');
    card.className = 'config-section';

    const header = document.createElement('div');
    header.innerHTML = `<div class="config-section-title">${getSectionLabel(section)}</div>`;
    card.appendChild(header);

    const grid = document.createElement('div');
    grid.className = 'config-grid';

    const keys = sortByOrder(Object.keys(items), keyOrder);

    keys.forEach(key => {
      const val = items[key];
      const text = getText(section, key);

      // Container
      const fieldCard = document.createElement('div');
      fieldCard.className = 'config-field';

      // Title
      const titleEl = document.createElement('div');
      titleEl.className = 'config-field-title';
      titleEl.textContent = text.title;
      fieldCard.appendChild(titleEl);

      // Description (Muted)
      const descEl = document.createElement('p');
      descEl.className = 'config-field-desc';
      descEl.textContent = text.desc;
      fieldCard.appendChild(descEl);

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
        built = buildSelectInput(section, key, 'url', [
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
      grid.appendChild(fieldCard);
    });

    card.appendChild(grid);

    if (grid.children.length > 0) {
      fragment.appendChild(card);
    }
  });

  container.appendChild(fragment);
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
        throw new Error('后台密码不能为空');
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
