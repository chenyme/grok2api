let apiKey = '';
let models = [];
let sending = false;

const WEBUI_THREADS_KEY = 'grok2api_webui_threads_v1';
const LEGACY_WEBUI_STATE_KEY = 'grok2api_webui_state_v1';
const byId = (id) => document.getElementById(id);

const TOKEN_RETRY_MAX = 12;
const TOKEN_RETRY_INTERVAL_MS = 1500;

const state = {
  threads: [],
  activeThreadId: null,
  ui: {
    model: '',
    mode: 'auto',
    stream: true,
    imageN: '1',
    imageSize: '1:1',
    videoRatio: '3:2',
    videoLength: '6'
  }
};

function sleep(ms) { return new Promise((resolve) => setTimeout(resolve, ms)); }

function now() { return Date.now(); }

function safeStorageGet(key) {
  try { return localStorage.getItem(key); } catch (_) { return null; }
}

function safeStorageSet(key, value) {
  try { localStorage.setItem(key, value); return true; } catch (_) { return false; }
}

function safeStorageRemove(key) {
  try { localStorage.removeItem(key); return true; } catch (_) { return false; }
}

function parseErrorMessage(message) {
  try {
    const parsed = JSON.parse(message || '{}');
    if (parsed?.error?.message) return String(parsed.error.message);
  } catch (_) {}
  return String(message || '请求失败');
}

function isNoTokenError(error) {
  const message = String(error?.message || '');
  try {
    const parsed = JSON.parse(message || '{}');
    const code = parsed?.error?.code;
    const type = parsed?.error?.type;
    const msg = String(parsed?.error?.message || '').toLowerCase();
    if ((code === 'rate_limit_exceeded' || type === 'rate_limit_error')
      && (msg.includes('no available tokens') || msg.includes('please try again later'))) {
      return true;
    }
  } catch (_) {}
  const lower = message.toLowerCase();
  return lower.includes('no available tokens') || lower.includes('rate_limit_exceeded');
}

async function requestWithTokenRetry(taskFn) {
  let lastError = null;
  for (let i = 1; i <= TOKEN_RETRY_MAX; i++) {
    try {
      return await taskFn();
    } catch (err) {
      lastError = err;
      if (!isNoTokenError(err)) throw err;
      if (i >= TOKEN_RETRY_MAX) break;
      if (typeof showToast === 'function') {
        showToast(`暂无可用 Token，正在重试 (${i}/${TOKEN_RETRY_MAX})`, 'warning');
      }
      await sleep(TOKEN_RETRY_INTERVAL_MS);
    }
  }
  throw new Error(`暂无可用 Token：${parseErrorMessage(lastError?.message)}`);
}

function esc(text) {
  return String(text)
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;')
    .replaceAll('"', '&quot;')
    .replaceAll("'", '&#39;');
}

function mdToHtml(text) {
  const input = text || '';
  if (window.marked?.parse) return window.marked.parse(input, { breaks: true, gfm: true });
  return esc(input).replace(/\n/g, '<br>');
}

function detectMode(modelId) {
  const lower = String(modelId || '').toLowerCase();
  if (lower.includes('imagine') || lower.includes('superimage')) return 'image';
  if (lower.includes('video')) return 'video';
  return 'chat';
}

function formatTime(ts) {
  try {
    return new Date(ts).toLocaleString('zh-CN', { month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit' });
  } catch (_) {
    return '';
  }
}

function shortTitle(text) {
  const cleaned = String(text || '').replace(/\s+/g, ' ').trim();
  if (!cleaned) return '新对话';
  return cleaned.length > 18 ? `${cleaned.slice(0, 18)}…` : cleaned;
}

function getActiveThread() {
  return state.threads.find((t) => t.id === state.activeThreadId) || null;
}

function createThread(initialTitle = '新对话') {
  const id = `thread_${Math.random().toString(36).slice(2, 10)}_${Date.now()}`;
  const t = {
    id,
    title: initialTitle,
    createdAt: now(),
    updatedAt: now(),
    messages: [],
    autoNamed: false
  };
  state.threads.unshift(t);
  state.activeThreadId = id;
  return t;
}

function ensureThread() {
  let t = getActiveThread();
  if (!t) t = createThread();
  return t;
}

function buildUiState() {
  return {
    model: byId('modelSelect')?.value || '',
    mode: byId('modeSelect')?.value || 'auto',
    stream: Boolean(byId('streamToggle')?.checked),
    imageN: byId('imageN')?.value || '1',
    imageSize: byId('imageSize')?.value || '1:1',
    videoRatio: byId('videoRatio')?.value || '3:2',
    videoLength: byId('videoLength')?.value || '6'
  };
}

function saveState() {
  state.ui = buildUiState();
  safeStorageSet(WEBUI_THREADS_KEY, JSON.stringify({
    savedAt: now(),
    threads: state.threads,
    activeThreadId: state.activeThreadId,
    ui: state.ui
  }));
}

function migrateLegacyState() {
  const raw = safeStorageGet(LEGACY_WEBUI_STATE_KEY);
  if (!raw) return;
  try {
    const old = JSON.parse(raw);
    if (!Array.isArray(old?.chatHistory) || !old.chatHistory.length) return;
    const t = createThread('历史会话');
    t.messages = old.chatHistory.map((m) => ({ role: m.role || 'assistant', content: m.content || '', ts: now() }));
    t.updatedAt = now();
    state.ui = { ...state.ui, ...(old.ui || {}) };
    saveState();
    safeStorageRemove(LEGACY_WEBUI_STATE_KEY);
  } catch (_) {}
}

function loadState() {
  migrateLegacyState();
  const raw = safeStorageGet(WEBUI_THREADS_KEY);
  if (!raw) return;
  try {
    const data = JSON.parse(raw);
    const threads = Array.isArray(data?.threads) ? data.threads : [];
    state.threads = threads.map((t) => ({
      id: t.id || `legacy_${Math.random().toString(36).slice(2, 10)}`,
      title: t.title || '新对话',
      createdAt: Number(t.createdAt || now()),
      updatedAt: Number(t.updatedAt || now()),
      autoNamed: Boolean(t.autoNamed),
      messages: Array.isArray(t.messages)
        ? t.messages.map((m) => ({ role: m.role || 'assistant', content: m.content || '', ts: Number(m.ts || now()) }))
        : []
    })).sort((a, b) => b.updatedAt - a.updatedAt);
    state.activeThreadId = data?.activeThreadId || state.threads[0]?.id || null;
    state.ui = { ...state.ui, ...(data?.ui || {}) };
  } catch (_) {}
}

function refreshOptionPanels() {
  const mode = byId('modeSelect').value;
  const model = byId('modelSelect').value;
  const resolved = mode === 'auto' ? detectMode(model) : mode;
  byId('imageOptions').classList.toggle('hidden', resolved !== 'image');
  byId('videoOptions').classList.toggle('hidden', resolved !== 'video');
}

function applyUiState() {
  const ui = state.ui || {};
  if (ui.mode && byId('modeSelect')) byId('modeSelect').value = ui.mode;
  if (typeof ui.stream === 'boolean') byId('streamToggle').checked = ui.stream;
  if (ui.imageN) byId('imageN').value = ui.imageN;
  if (ui.imageSize) byId('imageSize').value = ui.imageSize;
  if (ui.videoRatio) byId('videoRatio').value = ui.videoRatio;
  if (ui.videoLength) byId('videoLength').value = ui.videoLength;
  if (ui.model && byId('modelSelect')) {
    const exists = Array.from(byId('modelSelect').options).some((o) => o.value === ui.model);
    if (exists) byId('modelSelect').value = ui.model;
  }
  refreshOptionPanels();
}

function renderThreadList() {
  const box = byId('threadList');
  box.innerHTML = '';
  if (!state.threads.length) {
    const empty = document.createElement('div');
    empty.className = 'thread-meta';
    empty.textContent = '暂无历史对话，点击“新对话”开始。';
    box.appendChild(empty);
    return;
  }

  state.threads.forEach((t) => {
    const btn = document.createElement('button');
    btn.className = `thread-item ${t.id === state.activeThreadId ? 'active' : ''}`;
    btn.type = 'button';
    btn.innerHTML = `<div class="thread-title">${esc(t.title || '新对话')}</div>
      <div class="thread-meta">${t.messages.length} 条消息 · ${esc(formatTime(t.updatedAt))}</div>`;
    btn.addEventListener('click', () => {
      state.activeThreadId = t.id;
      renderAll();
      saveState();
    });
    box.appendChild(btn);
  });
}

function appendMessageNode(role, content, { returnNode = false } = {}) {
  const viewport = byId('chatViewport');
  const welcome = byId('welcomeCard');
  if (welcome) welcome.style.display = 'none';

  const row = document.createElement('div');
  row.className = `msg-row ${role}`;

  const bubble = document.createElement('div');
  bubble.className = 'msg-bubble';

  const roleLabel = document.createElement('div');
  roleLabel.className = 'msg-role';
  roleLabel.textContent = role;

  const contentNode = document.createElement('div');
  contentNode.className = 'msg-content';
  contentNode.innerHTML = mdToHtml(content);

  bubble.appendChild(roleLabel);
  bubble.appendChild(contentNode);
  row.appendChild(bubble);
  viewport.appendChild(row);
  viewport.scrollTop = viewport.scrollHeight;

  return returnNode ? contentNode : null;
}

function setStreamingNode(node, content) {
  if (!node) return;
  node.innerHTML = mdToHtml(content);
  const viewport = byId('chatViewport');
  viewport.scrollTop = viewport.scrollHeight;
}

function renderMessages() {
  const viewport = byId('chatViewport');
  const welcome = byId('welcomeCard');
  viewport.innerHTML = '';

  const t = ensureThread();
  if (!t.messages.length) {
    if (welcome) {
      viewport.appendChild(welcome);
      welcome.style.display = '';
    }
    return;
  }

  t.messages.forEach((msg) => appendMessageNode(msg.role || 'assistant', msg.content || ''));
}

function updateTopbar() {
  const t = ensureThread();
  byId('activeTitle').textContent = t.title || '新对话';
  byId('messageCount').textContent = `${t.messages.length} 条消息`;
}

function renderAll() {
  renderThreadList();
  renderMessages();
  updateTopbar();
}

function pushMessage(role, content) {
  const t = ensureThread();
  t.messages.push({ role, content, ts: now() });
  t.updatedAt = now();
  state.threads.sort((a, b) => b.updatedAt - a.updatedAt);
}

async function loadModels() {
  const res = await fetch('/v1/models', {
    headers: { ...buildAuthHeaders(apiKey), 'Content-Type': 'application/json' }
  });
  if (!res.ok) throw new Error('模型加载失败');
  const data = await res.json();
  models = (data.data || []).map((m) => m.id);

  const select = byId('modelSelect');
  select.innerHTML = '';
  for (const id of models) {
    const option = document.createElement('option');
    option.value = id;
    option.textContent = id;
    select.appendChild(option);
  }

  const prefer = state.ui.model || 'grok-4';
  if (models.includes(prefer)) select.value = prefer;
  else if (models.includes('grok-4')) select.value = 'grok-4';
  else if (models.length) select.value = models[0];
}

function buildChatPayload(model, stream, prompt, historyMessages) {
  const mode = byId('modeSelect').value;
  const resolved = mode === 'auto' ? detectMode(model) : mode;
  const messages = [...historyMessages, { role: 'user', content: prompt }];
  const payload = { model, stream, messages };

  if (resolved === 'video') {
    payload.video_config = {
      aspect_ratio: byId('videoRatio').value,
      video_length: Number(byId('videoLength').value),
      resolution_name: '480p',
      preset: 'custom'
    };
  }
  return payload;
}

function buildImagePayload(model, prompt) {
  return {
    model,
    prompt,
    n: Number(byId('imageN').value) || 1,
    size: byId('imageSize').value || '1:1',
    response_format: 'url',
    stream: false
  };
}

async function callChatNonStream(payload) {
  const res = await fetch('/v1/chat/completions', {
    method: 'POST',
    headers: { ...buildAuthHeaders(apiKey), 'Content-Type': 'application/json' },
    body: JSON.stringify(payload)
  });
  const text = await res.text();
  if (!res.ok) throw new Error(text || `请求失败(${res.status})`);
  const data = JSON.parse(text);
  return data?.choices?.[0]?.message?.content || text;
}

async function callChatStream(payload, onDelta) {
  const res = await fetch('/v1/chat/completions', {
    method: 'POST',
    headers: { ...buildAuthHeaders(apiKey), 'Content-Type': 'application/json' },
    body: JSON.stringify(payload)
  });
  if (!res.ok || !res.body) {
    const text = await res.text();
    throw new Error(text || `请求失败(${res.status})`);
  }

  const reader = res.body.getReader();
  const decoder = new TextDecoder('utf-8');
  let buffer = '';
  let assembled = '';

  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    buffer += decoder.decode(value, { stream: true });

    const blocks = buffer.split('\n\n');
    buffer = blocks.pop() || '';
    for (const block of blocks) {
      const lines = block.split('\n');
      for (const line of lines) {
        const trimmed = line.trim();
        if (!trimmed.startsWith('data:')) continue;
        const data = trimmed.slice(5).trim();
        if (!data || data === '[DONE]') continue;
        try {
          const obj = JSON.parse(data);
          const delta = obj?.choices?.[0]?.delta?.content;
          if (delta) {
            assembled += delta;
            onDelta(assembled);
          }
        } catch (_) {}
      }
    }
  }
  return assembled || '[empty stream]';
}

async function callImage(payload) {
  const res = await fetch('/v1/images/generations', {
    method: 'POST',
    headers: { ...buildAuthHeaders(apiKey), 'Content-Type': 'application/json' },
    body: JSON.stringify(payload)
  });
  const text = await res.text();
  if (!res.ok) throw new Error(text || `请求失败(${res.status})`);
  const data = JSON.parse(text);
  const urls = (data.data || []).map((it) => it.url).filter(Boolean);
  return urls.map((url, i) => `![image-${i + 1}](${url})`).join('\n') || text;
}

async function autoNameThread(thread, firstPrompt) {
  if (!thread || thread.autoNamed || thread.messages.length < 1) return;
  const base = shortTitle(firstPrompt);

  const candidateModel = models.find((m) => m === 'grok-3-mini')
    || models.find((m) => m.includes('grok-3-mini'))
    || models.find((m) => m.includes('mini'))
    || byId('modelSelect').value;
  if (!candidateModel) {
    thread.title = base;
    thread.autoNamed = true;
    renderAll();
    saveState();
    return;
  }

  try {
    const payload = {
      model: candidateModel,
      stream: false,
      messages: [
        { role: 'system', content: '你是标题助手。请根据用户首条问题生成一个简短中文标题，限制在12个字以内，只返回标题文本，不要任何解释或标点包装。' },
        { role: 'user', content: firstPrompt }
      ]
    };

    const titleRaw = await requestWithTokenRetry(() => callChatNonStream(payload));
    let title = String(titleRaw || '').replace(/[\n\r`"“”]/g, '').trim();
    if (!title) title = base;
    if (title.length > 14) title = `${title.slice(0, 14)}…`;
    thread.title = title;
  } catch (_) {
    thread.title = base;
  }

  thread.autoNamed = true;
  thread.updatedAt = now();
  state.threads.sort((a, b) => b.updatedAt - a.updatedAt);
  renderAll();
  saveState();
}

async function sendRequest() {
  if (sending) return;
  const promptInput = byId('promptInput');
  const prompt = promptInput.value.trim();
  if (!prompt) {
    if (typeof showToast === 'function') showToast('提示词不能为空', 'error');
    return;
  }

  const thread = ensureThread();
  const historyForApi = thread.messages
    .filter((m) => m.role === 'user' || m.role === 'assistant' || m.role === 'system')
    .map((m) => ({ role: m.role, content: m.content }));

  const model = byId('modelSelect').value;
  const stream = byId('streamToggle').checked;
  const mode = byId('modeSelect').value;
  const resolved = mode === 'auto' ? detectMode(model) : mode;

  sending = true;
  byId('sendBtn').disabled = true;

  pushMessage('user', prompt);
  appendMessageNode('user', prompt);
  promptInput.value = '';
  updateTopbar();
  saveState();

  try {
    let answer = '';

    if (resolved === 'image') {
      answer = await requestWithTokenRetry(() => callImage(buildImagePayload(model, prompt)));
      appendMessageNode('assistant(image)', answer);
    } else {
      const payload = buildChatPayload(model, stream, prompt, historyForApi);
      if (stream) {
        const node = appendMessageNode('assistant(stream)', '', { returnNode: true });
        answer = await requestWithTokenRetry(() => callChatStream(payload, (content) => setStreamingNode(node, content)));
        setStreamingNode(node, answer);
      } else {
        answer = await requestWithTokenRetry(() => callChatNonStream(payload));
        appendMessageNode('assistant', answer);
      }
    }

    pushMessage('assistant', answer);
    updateTopbar();
    renderThreadList();
    saveState();

    if (thread.messages.filter((m) => m.role === 'user').length === 1) {
      autoNameThread(thread, prompt);
    }
  } catch (e) {
    const msg = e.message || String(e);
    pushMessage('error', msg);
    appendMessageNode('error', msg);
    updateTopbar();
    renderThreadList();
    saveState();
    if (typeof showToast === 'function') showToast(msg, 'error');
  } finally {
    sending = false;
    byId('sendBtn').disabled = false;
    byId('promptInput').focus();
  }
}

function toggleSettingsPanel() {
  byId('settingsPanel').classList.toggle('hidden');
}

function bindUiEvents() {
  byId('newChatBtn').addEventListener('click', () => {
    createThread('新对话');
    renderAll();
    saveState();
    byId('promptInput').focus();
  });

  byId('clearCacheBtn').addEventListener('click', () => {
    if (!confirm('确定清空所有本地缓存会话吗？')) return;
    safeStorageRemove(WEBUI_THREADS_KEY);
    safeStorageRemove(LEGACY_WEBUI_STATE_KEY);
    state.threads = [];
    createThread('新对话');
    renderAll();
    saveState();
    if (typeof showToast === 'function') showToast('已清空本地缓存', 'success');
  });

  byId('sendBtn').addEventListener('click', sendRequest);
  byId('toggleSettingsBtn').addEventListener('click', toggleSettingsPanel);

  ['modelSelect', 'modeSelect', 'streamToggle', 'imageN', 'imageSize', 'videoRatio', 'videoLength'].forEach((id) => {
    byId(id).addEventListener('change', () => {
      refreshOptionPanels();
      saveState();
    });
  });

  byId('promptInput').addEventListener('keydown', (e) => {
    if (e.key !== 'Enter') return;
    if (e.ctrlKey) return; // Ctrl+Enter 换行
    e.preventDefault();
    sendRequest();
  });
}

async function bootstrap() {
  apiKey = await ensureApiKey();
  if (apiKey === null) return;

  bindUiEvents();
  loadState();
  if (!state.threads.length) createThread('新对话');

  try {
    await loadModels();
    applyUiState();
    renderAll();
    saveState();
  } catch (e) {
    renderAll();
    appendMessageNode('error', e.message || String(e));
    if (typeof showToast === 'function') showToast('模型加载失败', 'error');
  }
}

if (document.readyState === 'loading') {
  document.addEventListener('DOMContentLoaded', bootstrap);
} else {
  bootstrap();
}
