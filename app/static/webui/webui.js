let apiKey = '';
let models = [];

const byId = (id) => document.getElementById(id);

function escapeHtml(text) {
  return String(text)
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;')
    .replaceAll('"', '&quot;')
    .replaceAll("'", '&#39;');
}

function renderMarkdownImages(text) {
  const escaped = escapeHtml(text || '');
  return escaped.replace(/!\[([^\]]*)\]\((https?:\/\/[^)]+)\)/g, (_, alt, url) => {
    return `<div><img src="${url}" alt="${escapeHtml(alt)}" loading="lazy" /></div>`;
  }).replace(/\n/g, '<br>');
}

function appendResult(role, content) {
  const panel = byId('resultPanel');
  if (!panel) return;
  const item = document.createElement('div');
  item.className = 'result-item';
  item.innerHTML = `
    <div class="result-role">${escapeHtml(role)}</div>
    <div class="result-content">${renderMarkdownImages(content)}</div>
  `;
  panel.appendChild(item);
  panel.scrollTop = panel.scrollHeight;
}

function clearResult() {
  const panel = byId('resultPanel');
  if (panel) panel.innerHTML = '';
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

async function loadModels() {
  const res = await fetch('/v1/models', {
    headers: {
      ...buildAuthHeaders(apiKey),
      'Content-Type': 'application/json'
    }
  });
  if (!res.ok) throw new Error('模型加载失败');
  const data = await res.json();
  models = (data.data || []).map((m) => m.id);

  const select = byId('modelSelect');
  select.innerHTML = '';
  models.forEach((id) => {
    const option = document.createElement('option');
    option.value = id;
    option.textContent = id;
    select.appendChild(option);
  });

  if (models.includes('grok-4-1212')) {
    select.value = 'grok-4-1212';
  }
  refreshOptionPanels();
}

function buildChatPayload(model, stream) {
  const prompt = byId('promptInput').value.trim();
  if (!prompt) throw new Error('提示词不能为空');

  const mode = byId('modeSelect').value;
  const resolved = mode === 'auto' ? detectMode(model) : mode;

  const payload = {
    model,
    stream,
    messages: [{ role: 'user', content: prompt }]
  };

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

function buildImagePayload(model) {
  const prompt = byId('promptInput').value.trim();
  if (!prompt) throw new Error('提示词不能为空');

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
    headers: {
      ...buildAuthHeaders(apiKey),
      'Content-Type': 'application/json'
    },
    body: JSON.stringify(payload)
  });
  const text = await res.text();
  if (!res.ok) throw new Error(text || `请求失败(${res.status})`);

  const data = JSON.parse(text);
  const content = data?.choices?.[0]?.message?.content || text;
  appendResult('assistant', content);
}

async function callChatStream(payload) {
  const res = await fetch('/v1/chat/completions', {
    method: 'POST',
    headers: {
      ...buildAuthHeaders(apiKey),
      'Content-Type': 'application/json'
    },
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

    const lines = buffer.split('\n');
    buffer = lines.pop() || '';

    for (const line of lines) {
      const trimmed = line.trim();
      if (!trimmed.startsWith('data:')) continue;
      const data = trimmed.slice(5).trim();
      if (!data || data === '[DONE]') continue;

      try {
        const obj = JSON.parse(data);
        const delta = obj?.choices?.[0]?.delta?.content;
        if (delta) assembled += delta;
      } catch (_) {
        // ignore malformed chunk
      }
    }
  }

  appendResult('assistant(stream)', assembled || '[empty stream]');
}

async function callImage(payload) {
  const res = await fetch('/v1/images/generations', {
    method: 'POST',
    headers: {
      ...buildAuthHeaders(apiKey),
      'Content-Type': 'application/json'
    },
    body: JSON.stringify(payload)
  });
  const text = await res.text();
  if (!res.ok) throw new Error(text || `请求失败(${res.status})`);

  const data = JSON.parse(text);
  const urls = (data.data || []).map((item) => item.url).filter(Boolean);
  const md = urls.map((url, i) => `![image-${i + 1}](${url})`).join('\n');
  appendResult('image', md || text);
}

async function sendRequest() {
  const model = byId('modelSelect').value;
  const stream = byId('streamToggle').checked;
  const mode = byId('modeSelect').value;
  const resolved = mode === 'auto' ? detectMode(model) : mode;

  appendResult('user', byId('promptInput').value.trim());

  if (resolved === 'image') {
    await callImage(buildImagePayload(model));
    return;
  }

  const payload = buildChatPayload(model, stream);
  if (stream) {
    await callChatStream(payload);
  } else {
    await callChatNonStream(payload);
  }
}

async function bootstrap() {
  apiKey = await ensureApiKey();
  if (apiKey === null) return;

  byId('modelSelect').addEventListener('change', refreshOptionPanels);
  byId('modeSelect').addEventListener('change', refreshOptionPanels);

  byId('sendBtn').addEventListener('click', async () => {
    try {
      await sendRequest();
    } catch (e) {
      appendResult('error', e.message || String(e));
      if (typeof showToast === 'function') showToast(e.message || '请求失败', 'error');
    }
  });

  byId('clearBtn').addEventListener('click', clearResult);

  try {
    await loadModels();
  } catch (e) {
    appendResult('error', e.message || String(e));
    if (typeof showToast === 'function') showToast('模型加载失败', 'error');
  }
}

if (document.readyState === 'loading') {
  document.addEventListener('DOMContentLoaded', bootstrap);
} else {
  bootstrap();
}
