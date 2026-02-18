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
  'upload_concurrent',
  'upload_timeout',
  'download_concurrent',
  'download_timeout',
  'list_concurrent',
  'list_timeout',
  'list_batch_size',
  'delete_concurrent',
  'delete_timeout',
  'delete_batch_size',
  'reload_interval_sec',
  'stream_timeout',
  'final_timeout',
  'final_min_bytes',
  'medium_min_bytes',
  'concurrent',
  'batch_size'
]);

const LOCALE_MAP = {
  "app": {
    "label": "应用设置",
    "api_key": { title: "API 密钥", desc: "调用 Grok2API 服务的 Token（可选）。" },
    "app_key": { title: "后台密码", desc: "登录 Grok2API 管理后台的密码（必填）。" },
    "public_enabled": { title: "启用功能玩法", desc: "是否启用功能玩法入口（关闭则功能玩法页面不可访问）。" },
    "public_key": { title: "Public 密码", desc: "功能玩法页面的访问密码（可选）。" },
    "app_url": { title: "应用地址", desc: "当前 Grok2API 服务的外部访问 URL，用于文件链接访问。" },
    "image_format": { title: "图片格式", desc: "默认生成的图片格式（url 或 base64）。" },
    "video_format": { title: "视频格式", desc: "默认生成的视频格式（html 或 url，url 为处理后的链接）。" },
    "temporary": { title: "临时对话", desc: "是否默认启用临时对话模式。" },
    "disable_memory": { title: "禁用记忆", desc: "是否默认禁用 Grok 记忆功能。" },
    "stream": { title: "流式响应", desc: "是否默认启用流式输出。" },
    "thinking": { title: "思维链", desc: "是否默认启用思维链输出。" },
    "dynamic_statsig": { title: "动态指纹", desc: "是否默认启用动态生成 Statsig 指纹。" },
    "filter_tags": { title: "过滤标签", desc: "设置自动过滤 Grok 响应中的特殊标签。" }
  },


  "proxy": {
    "label": "代理配置",
    "base_proxy_url": { title: "基础代理 URL", desc: "代理请求到 Grok 官网的基础服务地址。" },
    "asset_proxy_url": { title: "资源代理 URL", desc: "代理请求到 Grok 官网的静态资源（图片/视频）地址。" },
    "cf_clearance": { title: "CF Clearance", desc: "Cloudflare Clearance Cookie，用于绕过反爬虫验证。" },
    "browser": { title: "浏览器指纹", desc: "curl_cffi 浏览器指纹标识（如 chrome136）。" },
    "user_agent": { title: "User-Agent", desc: "HTTP 请求的 User-Agent 字符串，需与浏览器指纹匹配。" }
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


  "chat": {
    "label": "对话配置",
    "concurrent": { title: "并发上限", desc: "Reverse 接口并发上限。" },
    "timeout": { title: "请求超时", desc: "Reverse 接口超时时间（秒）。" },
    "stream_timeout": { title: "流空闲超时", desc: "流式空闲超时时间（秒）。" }
  },


  "video": {
    "label": "视频配置",
    "concurrent": { title: "并发上限", desc: "Reverse 接口并发上限。" },
    "timeout": { title: "请求超时", desc: "Reverse 接口超时时间（秒）。" },
    "stream_timeout": { title: "流空闲超时", desc: "流式空闲超时时间（秒）。" }
  },


  "image": {
    "label": "图像配置",
    "timeout": { title: "请求超时", desc: "WebSocket 请求超时时间（秒）。" },
    "stream_timeout": { title: "流空闲超时", desc: "WebSocket 流式空闲超时时间（秒）。" },
    "final_timeout": { title: "最终图超时", desc: "收到中等图后等待最终图的超时秒数。" },
    "nsfw": { title: "NSFW 模式", desc: "WebSocket 请求是否启用 NSFW。" },
    "medium_min_bytes": { title: "中等图最小字节", desc: "判定中等质量图的最小字节数。" },
    "final_min_bytes": { title: "最终图最小字节", desc: "判定最终图的最小字节数（通常 JPG > 100KB）。" }
  },


  "asset": {
    "label": "资产配置",
    "upload_concurrent": { title: "上传并发", desc: "上传接口的最大并发数。推荐 30。" },
    "upload_timeout": { title: "上传超时", desc: "上传接口超时时间（秒）。推荐 60。" },
    "download_concurrent": { title: "下载并发", desc: "下载接口的最大并发数。推荐 30。" },
    "download_timeout": { title: "下载超时", desc: "下载接口超时时间（秒）。推荐 60。" },
    "list_concurrent": { title: "查询并发", desc: "资产查询接口的最大并发数。推荐 10。" },
    "list_timeout": { title: "查询超时", desc: "资产查询接口超时时间（秒）。推荐 60。" },
    "list_batch_size": { title: "查询批次大小", desc: "单次查询可处理的 Token 数量。推荐 10。" },
    "delete_concurrent": { title: "删除并发", desc: "资产删除接口的最大并发数。推荐 10。" },
    "delete_timeout": { title: "删除超时", desc: "资产删除接口超时时间（秒）。推荐 60。" },
    "delete_batch_size": { title: "删除批次大小", desc: "单次删除可处理的 Token 数量。推荐 10。" }
  },


  "voice": {
    "label": "语音配置",
    "timeout": { title: "请求超时", desc: "Voice 请求超时时间（秒）。" }
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


  "nsfw": {
    "label": "NSFW 配置",
    "concurrent": { title: "并发上限", desc: "批量开启 NSFW 模式时的并发请求上限。推荐 10。" },
    "batch_size": { title: "批次大小", desc: "批量开启 NSFW 模式的单批处理数量。推荐 50。" },
    "timeout": { title: "请求超时", desc: "NSFW 开启相关请求的超时时间（秒）。推荐 60。" }
  },


  "usage": {
    "label": "Usage 配置",
    "concurrent": { title: "并发上限", desc: "批量刷新用量时的并发请求上限。推荐 10。" },
    "batch_size": { title: "批次大小", desc: "批量刷新用量的单批处理数量。推荐 50。" },
    "timeout": { title: "请求超时", desc: "用量查询接口的超时时间（秒）。推荐 60。" }
  }
};

// 配置部分说明（可选）
const SECTION_DESCRIPTIONS = {
  "proxy": "配置不正确将导致 403 错误。服务首次请求 Grok 时的 IP 必须与获取 CF Clearance 时的 IP 一致，后续服务器请求 IP 变化不会导致 403。"
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

  const genBtn = document.createElement('button');
  genBtn.className = 'flex-none w-[32px] h-[32px] flex items-center justify-center bg-black text-white rounded-md hover:opacity-80 transition-opacity';
  genBtn.type = 'button';
  genBtn.title = '生成';
  genBtn.innerHTML = `<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 12a9 9 0 1 1-3-6.7"/><polyline points="21 3 21 9 15 9"/></svg>`;
  genBtn.onclick = () => {
    input.value = randomKey(16);
  };

  const copyBtn = document.createElement('button');
  copyBtn.className = 'flex-none w-[32px] h-[32px] flex items-center justify-center bg-black text-white rounded-md hover:opacity-80 transition-opacity';
  copyBtn.type = 'button';
  copyBtn.innerHTML = `<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="13" height="13" rx="2" ry="2"></rect><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"></path></svg>`;
  copyBtn.onclick = () => copyToClipboard(input.value, copyBtn);

  wrapper.appendChild(input);
  wrapper.appendChild(genBtn);
  wrapper.appendChild(copyBtn);

  return { input, node: wrapper };
}

function randomKey(len) {
  const chars = 'abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789';
  const out = [];
  if (window.crypto && window.crypto.getRandomValues) {
    const buf = new Uint8Array(len);
    window.crypto.getRandomValues(buf);
    for (let i = 0; i < len; i++) {
      out.push(chars[buf[i] % chars.length]);
    }
    return out.join('');
  }
  for (let i = 0; i < len; i++) {
    out.push(chars[Math.floor(Math.random() * chars.length)]);
  }
  return out.join('');
}

async function init() {
  apiKey = await ensureAdminKey();
  if (apiKey === null) return;
  loadData();
}

async function loadData() {
  try {
    const res = await fetch('/v1/admin/config', {
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
    if (key === 'api_key' || key === 'app_key' || key === 'public_key') {
      built = buildSecretInput(section, key, val);
    } else {
      built = buildTextInput(section, key, val);
    }
  }

  if (built) {
    inputWrapper.appendChild(built.node);
  }
  fieldCard.appendChild(inputWrapper);

  if (section === 'app' && key === 'public_enabled') {
    fieldCard.classList.add('has-action');
    const link = document.createElement('a');
    link.href = '/login';
    link.className = 'config-field-action flex-none w-[32px] h-[32px] flex items-center justify-center bg-black text-white rounded-md hover:opacity-80 transition-opacity';
    link.title = '功能玩法';
    link.setAttribute('aria-label', '功能玩法');
    link.innerHTML = `<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M7 17L17 7"/><path d="M7 7h10v10"/></svg>`;
    link.style.display = val ? 'inline-flex' : 'none';
    fieldCard.appendChild(link);
    if (built && built.input) {
      built.input.addEventListener('change', () => {
        link.style.display = built.input.checked ? 'inline-flex' : 'none';
      });
    }
  }

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

    const res = await fetch('/v1/admin/config', {
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
