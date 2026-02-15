let apiKey = '';
let models = [];
let chatHistory = [];
let sending = false;

const WEBUI_STATE_KEY = 'grok2api_webui_state_v1';
const byId = (id) => document.getElementById(id);

const TOKEN_RETRY_MAX = 12;
const TOKEN_RETRY_INTERVAL_MS = 1500;

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function parseErrorMessage(message) {
  try {
    const parsed = JSON.parse(message || '{}');
    if (parsed?.error?.message) return String(parsed.error.message);
  } catch (_) {
    // ignore
  }
  return String(message || '请求失败');
}

function isNoTokenError(error) {
  const message = String(error?.message || '');
  try {
    const parsed = JSON.parse(message || '{}');
    const code = parsed?.error?.code;
    const type = parsed?.error?.type;
    const msg = String(parsed?.error?.message || '').toLowerCase();
    if (code === 'rate_limit_exceeded' || type === 'rate_limit_error') {
      if (msg.includes('no available tokens') || msg.includes('please try again later')) {
        return true;
      }
    }
  } catch (_) {
    // ignore
  }

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

  const msg = parseErrorMessage(lastError?.message || 'No available tokens.');
  throw new Error(`暂无可用 Token：${msg}`);
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
  if (window.marked?.parse) {
    return window.marked.parse(input, { breaks: true, gfm: true });
  }
  return esc(input).replace(/\n/g, '<br>');
}

function detectMode(modelId) {
  const lower = String(modelId || '').toLowerCase();
  if (lower.includes('imagine') || lower.includes('superimage')) return 'image';
  if (lower.includes('video')) return 'video';
  return 'chat';
}

function updateMessageCount() {
  const el = byId('messageCount');
  if (!el) return;
  el.textContent = `${chatHistory.length} 条消息`;
}

function getUiState() {
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

function saveWebuiState() {
  try {
    const payload = { savedAt: Date.now(), chatHistory, ui: getUiState() };
    localStorage.setItem(WEBUI_STATE_KEY, JSON.stringify(payload));
  } catch (_) {
    // ignore
  }
}

function loadWebuiState() {
  try {
    const raw = localStorage.getItem(WEBUI_STATE_KEY);
    if (!raw) return null;
    return JSON.parse(raw);
  } catch (_) {
    return null;
  }
}

function refreshOptionPanels() {
  const mode = byId('modeSelect').value;
  const model = byId('modelSelect').value;
  const resolved = mode === 'auto' ? detectMode(model) : mode;
  byId('imageOptions').classList.toggle('hidden', resolved !== 'image');
  byId('videoOptions').classList.toggle('hidden', resolved !== 'video');
}

function appendMessage(role, content, options = {}) {
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

  if (options.returnNode) return contentNode;
  return null;
}

function setStreamingNode(node, content) {
  if (!node) return;
  node.innerHTML = mdToHtml(content);
  const viewport = byId('chatViewport');
  viewport.scrollTop = viewport.scrollHeight;
}

function clearViewportOnly() {
  const viewport = byId('chatViewport');
  viewport.innerHTML = '';
  const welcome = byId('welcomeCard');
  if (welcome) {
    viewport.appendChild(welcome);
    welcome.style.display = '';
  }
  updateMessageCount();
}

function renderHistory() {
  clearViewportOnly();
  for (const item of chatHistory) {
    appendMessage(item?.role || 'assistant', item?.content || '');
  }
  updateMessageCount();
}

function applyUiState(ui) {
  if (!ui) return;
  if (ui.mode && byId('modeSelect')) byId('modeSelect').value = ui.mode;
  if (typeof ui.stream === 'boolean' && byId('streamToggle')) byId('streamToggle').checked = ui.stream;
  if (ui.imageN && byId('imageN')) byId('imageN').value = ui.imageN;
  if (ui.imageSize && byId('imageSize')) byId('imageSize').value = ui.imageSize;
  if (ui.videoRatio && byId('videoRatio')) byId('videoRatio').value = ui.videoRatio;
  if (ui.videoLength && byId('videoLength')) byId('videoLength').value = ui.videoLength;
  if (ui.model && byId('modelSelect')) {
    const exists = Array.from(byId('modelSelect').options).some((o) => o.value === ui.model);
    if (exists) byId('modelSelect').value = ui.model;
  }
  refreshOptionPanels();
}

function resetConversation() {
  chatHistory = [];
  clearViewportOnly();
  saveWebuiState();
  if (typeof showToast === 'function') showToast('已创建新对话', 'success');
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
  if (models.includes('grok-4')) select.value = 'grok-4';
  else if (models.includes('grok-4-1212')) select.value = 'grok-4-1212';
  refreshOptionPanels();
}

function buildChatPayload(model, stream, prompt) {
  const mode = byId('modeSelect').value;
  const resolved = mode === 'auto' ? detectMode(model) : mode;

  const messages = [...chatHistory, { role: 'user', content: prompt }];
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

async function callChatStream(payload) {
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
  const node = appendMessage('assistant(stream)', '', { returnNode: true });

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
            setStreamingNode(node, assembled);
          }
        } catch (_) {
          // ignore malformed chunk
        }
      }
    }
  }

  if (!assembled) setStreamingNode(node, '[empty stream]');
  return assembled;
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

async function sendRequest() {
  if (sending) return;
  const prompt = byId('promptInput').value.trim();
  if (!prompt) {
    if (typeof showToast === 'function') showToast('提示词不能为空', 'error');
    return;
  }

  const model = byId('modelSelect').value;
  const stream = byId('streamToggle').checked;
  const mode = byId('modeSelect').value;
  const resolved = mode === 'auto' ? detectMode(model) : mode;

  sending = true;
  byId('sendBtn').disabled = true;

  appendMessage('user', prompt);
  byId('promptInput').value = '';

  try {
    let answer = '';
    if (resolved === 'image') {
      answer = await requestWithTokenRetry(() => callImage(buildImagePayload(model, prompt)));
      appendMessage('assistant(image)', answer);
    } else {
      const payload = buildChatPayload(model, stream, prompt);
      answer = await requestWithTokenRetry(() => (stream ? callChatStream(payload) : callChatNonStream(payload)));
      if (!stream) appendMessage('assistant', answer);
    }

    chatHistory.push({ role: 'user', content: prompt });
    if (answer) chatHistory.push({ role: 'assistant', content: answer });
    updateMessageCount();
    saveWebuiState();
  } catch (e) {
    appendMessage('error', e.message || String(e));
    if (typeof showToast === 'function') showToast(e.message || '请求失败', 'error');
  } finally {
    sending = false;
    byId('sendBtn').disabled = false;
    byId('promptInput').focus();
  }
}

async function bootstrap() {
  apiKey = await ensureApiKey();
  if (apiKey === null) return;
  updateMessageCount();

  byId('modelSelect').addEventListener('change', () => {
    refreshOptionPanels();
    saveWebuiState();
  });
  byId('modeSelect').addEventListener('change', () => {
    refreshOptionPanels();
    saveWebuiState();
  });

  byId('sendBtn').addEventListener('click', sendRequest);
  byId('clearBtn').addEventListener('click', clearViewportOnly);
  byId('newChatBtn').addEventListener('click', resetConversation);
  byId('restoreBtn').addEventListener('click', () => {
    const state = loadWebuiState();
    if (!state) {
      if (typeof showToast === 'function') showToast('没有可恢复的暂存', 'warning');
      return;
    }
    chatHistory = Array.isArray(state.chatHistory) ? state.chatHistory : [];
    applyUiState(state.ui || {});
    renderHistory();
    if (typeof showToast === 'function') showToast('已恢复暂存对话', 'success');
  });
  byId('clearCacheBtn').addEventListener('click', () => {
    localStorage.removeItem(WEBUI_STATE_KEY);
    if (typeof showToast === 'function') showToast('已清空浏览器暂存', 'success');
  });

  byId('streamToggle').addEventListener('change', saveWebuiState);
  byId('imageN').addEventListener('change', saveWebuiState);
  byId('imageSize').addEventListener('change', saveWebuiState);
  byId('videoRatio').addEventListener('change', saveWebuiState);
  byId('videoLength').addEventListener('change', saveWebuiState);

  byId('promptInput').addEventListener('keydown', (e) => {
    if (e.key !== 'Enter') return;

    // Ctrl + Enter: 换行
    if (e.ctrlKey) {
      return;
    }

    // Enter: 发送
    e.preventDefault();
    sendRequest();
  });

  try {
    await loadModels();
    const state = loadWebuiState();
    if (state) {
      chatHistory = Array.isArray(state.chatHistory) ? state.chatHistory : [];
      applyUiState(state.ui || {});
      renderHistory();
    }
  } catch (e) {
    appendMessage('error', e.message || String(e));
    if (typeof showToast === 'function') showToast('模型加载失败', 'error');
  }
}

if (document.readyState === 'loading') {
  document.addEventListener('DOMContentLoaded', bootstrap);
} else {
  bootstrap();
}
