(() => {
  const isAdminPage = window.location.pathname.startsWith('/admin');
  const isEmbedded = new URLSearchParams(window.location.search).get('embed') === '1';
  const VERIFY_ENDPOINT = isAdminPage ? `${ADMIN_API}/verify` : '/webui/api/verify';
  const IMAGE_ENDPOINT = isAdminPage ? `${ADMIN_API}/images/generations` : '/webui/api/images/generations';
  const keyStore = isAdminPage ? adminKey : webuiKey;
  const loginPath = isAdminPage ? '/admin/login' : '/webui/login';
  const promptInput = document.getElementById('promptInput');
  const modelSelect = document.getElementById('modelSelect');
  const sizeSelect = document.getElementById('sizeSelect');
  const countInput = document.getElementById('countInput');
  const modeSelect = document.getElementById('modeSelect');
  const formatSelect = document.getElementById('formatSelect');
  const generateBtn = document.getElementById('generateBtn');
  const stopBtn = document.getElementById('stopBtn');
  const clearHistoryBtn = document.getElementById('clearHistoryBtn');
  const historyEl = document.getElementById('resultHistory');
  const emptyEl = document.getElementById('imageEmpty');
  const statusEl = document.getElementById('imageStatus');

  let key = '';
  let busy = false;
  let stopRequested = false;
  let batchCounter = 0;
  let historyCount = 0;

  function toast(message, type = 'info') {
    if (typeof showToast === 'function') showToast(message, type);
  }

  function setStatus(message, state = 'ready') {
    if (!statusEl) return;
    statusEl.textContent = message;
    statusEl.dataset.state = state;
  }

  function setBusy(next) {
    busy = next;
    [promptInput, modelSelect, sizeSelect, countInput, modeSelect, formatSelect, generateBtn].forEach((el) => {
      if (el) el.disabled = next;
    });
    if (generateBtn) generateBtn.textContent = next ? '生成中…' : '生成图片';
    if (stopBtn) stopBtn.disabled = !next;
    syncEmptyState();
  }

  function syncEmptyState() {
    if (emptyEl) emptyEl.hidden = historyCount > 0 || busy;
    if (clearHistoryBtn) clearHistoryBtn.disabled = historyCount === 0 || busy;
  }

  function bearerHeaders() {
    return key ? { Authorization: `Bearer ${key}` } : {};
  }

  function normalizeImageUrl(url) {
    if (!url) return '';
    try { return new URL(url, window.location.origin).toString(); } catch { return url; }
  }

  function sizeToRatio(size) {
    const option = Array.from(sizeSelect?.options || []).find((item) => item.value === size);
    return option?.dataset?.ratio || '1:1';
  }

  function ratioToAspect(ratio) {
    const [width, height] = String(ratio || '1:1').split(':').map((item) => Number(item) || 1);
    return `${width} / ${height}`;
  }

  function downloadDataUrl(dataUrl, filename) {
    const link = document.createElement('a');
    link.href = dataUrl;
    link.download = filename;
    document.body.appendChild(link);
    link.click();
    link.remove();
  }

  async function copyText(text, message) {
    await navigator.clipboard?.writeText(text);
    toast(message, 'success');
  }

  function getPayloadFromControls() {
    const selectedSize = sizeSelect?.selectedOptions?.[0];
    return {
      model: modelSelect?.value || 'grok-imagine-image-lite',
      prompt: (promptInput?.value || '').trim(),
      n: Math.max(1, Math.min(4, Number(countInput?.value || 1))),
      size: sizeSelect?.value || '1024x1024',
      aspect_ratio: selectedSize?.dataset?.ratio || undefined,
      response_format: formatSelect?.value || 'url',
    };
  }

  function createBatch(payload, state = 'generating') {
    batchCounter += 1;
    historyCount += 1;
    syncEmptyState();

    const batch = document.createElement('article');
    batch.className = 'webui-images-batch';
    batch.dataset.batchId = String(batchCounter);
    batch.dataset.state = state;

    const head = document.createElement('div');
    head.className = 'webui-images-batch-head';

    const promptWrap = document.createElement('div');
    promptWrap.className = 'webui-images-batch-prompt-wrap';

    const prompt = document.createElement('div');
    prompt.className = 'webui-images-batch-prompt';
    prompt.textContent = payload.prompt;
    promptWrap.appendChild(prompt);

    const controls = document.createElement('div');
    controls.className = 'webui-images-batch-controls';

    const retry = document.createElement('button');
    retry.type = 'button';
    retry.className = 'btn btn-ghost webui-images-small-btn';
    retry.textContent = '重试';
    retry.addEventListener('click', () => runBatch({ ...payload }, { retry: true }));
    controls.appendChild(retry);

    const copyPrompt = document.createElement('button');
    copyPrompt.type = 'button';
    copyPrompt.className = 'btn btn-ghost webui-images-small-btn';
    copyPrompt.textContent = '复制 Prompt';
    copyPrompt.addEventListener('click', () => copyText(payload.prompt, '已复制 Prompt'));
    controls.appendChild(copyPrompt);

    promptWrap.appendChild(controls);
    head.appendChild(promptWrap);

    const meta = document.createElement('div');
    meta.className = 'webui-images-batch-meta';
    [
      `#${batchCounter}`,
      payload.model,
      payload.size,
      payload.aspect_ratio || sizeToRatio(payload.size),
      `${payload.n} 张`,
      payload.response_format,
    ].forEach((label, index) => {
      const chip = document.createElement('span');
      chip.className = `webui-images-batch-chip${index === 0 ? ' is-round' : ''}`;
      chip.textContent = label;
      meta.appendChild(chip);
    });

    const stateChip = document.createElement('span');
    stateChip.className = 'webui-images-batch-chip is-state';
    stateChip.dataset.state = state;
    stateChip.textContent = state === 'failed' ? '失败' : '生成中';
    meta.appendChild(stateChip);
    head.appendChild(meta);
    batch.appendChild(head);

    const grid = document.createElement('div');
    grid.className = 'webui-images-grid';
    grid.style.setProperty('--tile-aspect', ratioToAspect(payload.aspect_ratio || sizeToRatio(payload.size)));
    Array.from({ length: payload.n }, (_, index) => {
      grid.appendChild(createPendingTile(index));
    });
    batch.appendChild(grid);

    historyEl?.prepend(batch);
    return { batch, grid, stateChip };
  }

  function createPendingTile(index) {
    const card = document.createElement('article');
    card.className = 'webui-images-card is-pending';

    const body = document.createElement('div');
    body.className = 'webui-images-card-body';

    const label = document.createElement('span');
    label.className = 'webui-images-card-label';
    label.textContent = `#${index + 1}`;
    body.appendChild(label);

    card.appendChild(body);
    return card;
  }

  function createImageTile(item, index, payload) {
    const card = document.createElement('article');
    card.className = 'webui-images-card is-ready';

    const body = document.createElement('div');
    body.className = 'webui-images-card-body';
    const actions = document.createElement('div');
    actions.className = 'webui-images-card-actions';

    if (payload.response_format === 'b64_json' && item.b64_json) {
      const dataUrl = `data:image/png;base64,${item.b64_json}`;
      const img = document.createElement('img');
      img.alt = `Generated image ${index + 1}`;
      img.loading = 'lazy';
      img.src = dataUrl;
      body.appendChild(img);

      const download = document.createElement('button');
      download.type = 'button';
      download.className = 'btn btn-ghost webui-images-small-btn';
      download.textContent = '下载';
      download.addEventListener('click', () => downloadDataUrl(dataUrl, `grok-image-${Date.now()}-${index + 1}.png`));
      actions.appendChild(download);

      const copy = document.createElement('button');
      copy.type = 'button';
      copy.className = 'btn btn-ghost webui-images-small-btn';
      copy.textContent = '复制 Base64';
      copy.addEventListener('click', () => copyText(item.b64_json, '已复制 Base64'));
      actions.appendChild(copy);
    } else if (item.url) {
      const url = normalizeImageUrl(item.url);
      const link = document.createElement('a');
      link.href = url;
      link.target = '_blank';
      link.rel = 'noopener';
      link.className = 'webui-images-card-link';
      const img = document.createElement('img');
      img.alt = `Generated image ${index + 1}`;
      img.loading = 'lazy';
      img.src = url;
      link.appendChild(img);
      body.appendChild(link);

      const open = document.createElement('a');
      open.className = 'btn btn-ghost webui-images-small-btn';
      open.href = url;
      open.target = '_blank';
      open.rel = 'noopener';
      open.download = `grok-image-${Date.now()}-${index + 1}.png`;
      open.textContent = '下载/打开';
      actions.appendChild(open);

      const copy = document.createElement('button');
      copy.type = 'button';
      copy.className = 'btn btn-ghost webui-images-small-btn';
      copy.textContent = '复制 URL';
      copy.addEventListener('click', () => copyText(url, '已复制 URL'));
      actions.appendChild(copy);
    } else {
      const pre = document.createElement('pre');
      pre.textContent = JSON.stringify(item, null, 2);
      body.appendChild(pre);
    }

    const badge = document.createElement('span');
    badge.className = 'webui-images-card-badge';
    badge.textContent = `#${index + 1}`;
    body.appendChild(badge);
    card.appendChild(body);
    if (actions.childElementCount) card.appendChild(actions);
    return card;
  }

  function setBatchState(batchParts, state, label) {
    batchParts.batch.dataset.state = state;
    batchParts.stateChip.dataset.state = state;
    batchParts.stateChip.textContent = label;
  }

  function renderError(batchParts, message) {
    batchParts.grid.innerHTML = '';
    const pre = document.createElement('pre');
    pre.className = 'webui-images-error';
    pre.textContent = message;
    batchParts.grid.appendChild(pre);
    setBatchState(batchParts, 'failed', '失败');
  }

  async function runBatch(payload, options = {}) {
    if (busy && !options.fromLoop) {
      toast('当前仍在生成中', 'error');
      return false;
    }
    const batchParts = createBatch(payload);
    try {
      const res = await fetch(IMAGE_ENDPOINT, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', ...bearerHeaders() },
        body: JSON.stringify(payload),
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) throw new Error(data?.error?.message || data?.detail || `HTTP ${res.status}`);
      const items = Array.isArray(data?.data) ? data.data : [];
      if (!items.length) throw new Error('响应中没有图片数据');
      batchParts.grid.innerHTML = '';
      items.forEach((item, index) => batchParts.grid.appendChild(createImageTile(item, index, payload)));
      setBatchState(batchParts, items.length === payload.n ? 'success' : 'partial', items.length === payload.n ? '完成' : `完成 ${items.length}/${payload.n}`);
      setStatus(options.retry ? '重试完成' : '完成', 'success');
      return true;
    } catch (err) {
      const message = err?.message || String(err || '请求失败');
      renderError(batchParts, message);
      setStatus('失败', 'error');
      toast(message, 'error');
      return false;
    }
  }

  async function generate() {
    if (busy) return;
    const payload = getPayloadFromControls();
    if (!payload.prompt) {
      toast('请输入 prompt', 'error');
      promptInput?.focus();
      return;
    }

    stopRequested = false;
    setBusy(true);
    setStatus('生成中…', 'running');
    try {
      if (modeSelect?.value === 'continuous') {
        while (!stopRequested) {
          const ok = await runBatch({ ...payload }, { fromLoop: true });
          if (!ok) break;
        }
        if (stopRequested) setStatus('已停止', 'ready');
      } else {
        await runBatch(payload, { fromLoop: true });
      }
    } finally {
      setBusy(false);
    }
  }

  async function init() {
    const header = document.getElementById('image-header');
    if (header) {
      header.id = isAdminPage ? 'admin-header' : 'webui-header';
      header.dataset.active = isAdminPage ? '/admin/images' : '/webui/images';
    }
    if (isEmbedded) {
      document.body.classList.add('webui-embedded-page');
      header?.remove();
    } else if (isAdminPage) {
      await window.renderAdminHeader?.();
    } else {
      await window.renderWebuiHeader?.();
    }
    await window.renderSiteFooter?.();
    key = await keyStore.get();
    try {
      const ok = await verifyKey(VERIFY_ENDPOINT, key);
      if (!ok) {
        keyStore.clear();
        location.href = loginPath;
        return;
      }
    } catch {
      location.href = loginPath;
      return;
    }
    generateBtn?.addEventListener('click', generate);
    stopBtn?.addEventListener('click', () => {
      stopRequested = true;
      if (stopBtn) stopBtn.disabled = true;
      setStatus('停止中…', 'running');
    });
    clearHistoryBtn?.addEventListener('click', () => {
      if (busy || !historyEl) return;
      historyEl.innerHTML = '';
      historyCount = 0;
      setStatus('就绪', 'ready');
      syncEmptyState();
    });
    promptInput?.addEventListener('keydown', (event) => {
      if (event.key === 'Enter' && (event.ctrlKey || event.metaKey)) generate();
    });
    syncEmptyState();
  }

  init().catch(() => {
    setStatus('初始化失败', 'error');
  });
})();
