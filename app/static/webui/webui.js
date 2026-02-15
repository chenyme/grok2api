let apiKey = '';
let models = [];
let chatHistory = [];
let sending = false;

const byId = (id) => document.getElementById(id);

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
}

function resetConversation() {
  chatHistory = [];
  clearViewportOnly();
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

  if (!assembled) {
    setStreamingNode(node, '[empty stream]');
  }

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
      answer = await callImage(buildImagePayload(model, prompt));
      appendMessage('assistant(image)', answer);
    } else {
      const payload = buildChatPayload(model, stream, prompt);
      answer = stream ? await callChatStream(payload) : await callChatNonStream(payload);
      if (!stream) appendMessage('assistant', answer);
    }

    chatHistory.push({ role: 'user', content: prompt });
    if (answer) chatHistory.push({ role: 'assistant', content: answer });
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

  byId('modelSelect').addEventListener('change', refreshOptionPanels);
  byId('modeSelect').addEventListener('change', refreshOptionPanels);

  byId('sendBtn').addEventListener('click', sendRequest);
  byId('clearBtn').addEventListener('click', clearViewportOnly);
  byId('newChatBtn').addEventListener('click', resetConversation);

  byId('promptInput').addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && e.ctrlKey) {
      e.preventDefault();
      sendRequest();
    }
  });

  try {
    await loadModels();
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
