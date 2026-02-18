(() => {
  const startBtn = document.getElementById('startBtn');
  const stopBtn = document.getElementById('stopBtn');
  const clearBtn = document.getElementById('clearBtn');
  const promptInput = document.getElementById('promptInput');
  const imageUrlInput = document.getElementById('imageUrlInput');
  const imageFileInput = document.getElementById('imageFileInput');
  const imageFileName = document.getElementById('imageFileName');
  const clearImageFileBtn = document.getElementById('clearImageFileBtn');
  const selectImageFileBtn = document.getElementById('selectImageFileBtn');
  const ratioSelect = document.getElementById('ratioSelect');
  const lengthSelect = document.getElementById('lengthSelect');
  const resolutionSelect = document.getElementById('resolutionSelect');
  const presetSelect = document.getElementById('presetSelect');
  const statusText = document.getElementById('statusText');
  const progressBar = document.getElementById('progressBar');
  const progressFill = document.getElementById('progressFill');
  const progressText = document.getElementById('progressText');
  const durationValue = document.getElementById('durationValue');
  const aspectValue = document.getElementById('aspectValue');
  const lengthValue = document.getElementById('lengthValue');
  const resolutionValue = document.getElementById('resolutionValue');
  const presetValue = document.getElementById('presetValue');
  const videoEmpty = document.getElementById('videoEmpty');
  const videoStage = document.getElementById('videoStage');

  let currentSource = null;
  let currentTaskId = '';
  let isRunning = false;
  let progressBuffer = '';
  let contentBuffer = '';
  let collectingContent = false;
  let startAt = 0;
  let fileDataUrl = '';
  let elapsedTimer = null;
  let lastProgress = 0;
  let currentPreviewItem = null;
  let previewCount = 0;
  const DEFAULT_REASONING_EFFORT = 'low';

  function toast(message, type) {
    if (typeof showToast === 'function') {
      showToast(message, type);
    }
  }

  function setStatus(state, text) {
    if (!statusText) return;
    statusText.textContent = text;
    statusText.classList.remove('connected', 'connecting', 'error');
    if (state) {
      statusText.classList.add(state);
    }
  }

  function setButtons(running) {
    if (!startBtn || !stopBtn) return;
    if (running) {
      startBtn.classList.add('hidden');
      stopBtn.classList.remove('hidden');
    } else {
      startBtn.classList.remove('hidden');
      stopBtn.classList.add('hidden');
      startBtn.disabled = false;
    }
  }

  function updateProgress(value) {
    const safe = Math.max(0, Math.min(100, Number(value) || 0));
    lastProgress = safe;
    if (progressFill) {
      progressFill.style.width = `${safe}%`;
    }
    if (progressText) {
      progressText.textContent = `${safe}%`;
    }
  }

  function updateMeta() {
    if (aspectValue && ratioSelect) {
      aspectValue.textContent = ratioSelect.value;
    }
    if (lengthValue && lengthSelect) {
      lengthValue.textContent = `${lengthSelect.value}s`;
    }
    if (resolutionValue && resolutionSelect) {
      resolutionValue.textContent = resolutionSelect.value;
    }
    if (presetValue && presetSelect) {
      presetValue.textContent = presetSelect.value;
    }
  }

  function resetOutput(keepPreview) {
    progressBuffer = '';
    contentBuffer = '';
    collectingContent = false;
    lastProgress = 0;
    currentPreviewItem = null;
    updateProgress(0);
    setIndeterminate(false);
    if (!keepPreview) {
      if (videoStage) {
        videoStage.innerHTML = '';
        videoStage.classList.add('hidden');
      }
      if (videoEmpty) {
        videoEmpty.classList.remove('hidden');
      }
      previewCount = 0;
    }
    if (durationValue) {
      durationValue.textContent = '耗时 -';
    }
  }

  function initPreviewSlot() {
    if (!videoStage) return;
    previewCount += 1;
    currentPreviewItem = document.createElement('div');
    currentPreviewItem.className = 'video-item';
    currentPreviewItem.dataset.index = String(previewCount);
    currentPreviewItem.classList.add('is-pending');

    const header = document.createElement('div');
    header.className = 'video-item-bar';

    const title = document.createElement('div');
    title.className = 'video-item-title';
    title.textContent = `视频 ${previewCount}`;

    const actions = document.createElement('div');
    actions.className = 'video-item-actions';

    const openBtn = document.createElement('a');
    openBtn.className = 'geist-button-outline text-xs px-3 video-open hidden';
    openBtn.target = '_blank';
    openBtn.rel = 'noopener';
    openBtn.textContent = '打开';

    const downloadBtn = document.createElement('button');
    downloadBtn.className = 'geist-button-outline text-xs px-3 video-download';
    downloadBtn.type = 'button';
    downloadBtn.textContent = '下载';
    downloadBtn.disabled = true;

    actions.appendChild(openBtn);
    actions.appendChild(downloadBtn);
    header.appendChild(title);
    header.appendChild(actions);

    const body = document.createElement('div');
    body.className = 'video-item-body';
    body.innerHTML = '<div class="video-item-placeholder">生成中…</div>';

    const link = document.createElement('div');
    link.className = 'video-item-link';

    currentPreviewItem.appendChild(header);
    currentPreviewItem.appendChild(body);
    currentPreviewItem.appendChild(link);
    videoStage.appendChild(currentPreviewItem);
    videoStage.classList.remove('hidden');
    if (videoEmpty) {
      videoEmpty.classList.add('hidden');
    }
  }

  function ensurePreviewSlot() {
    if (!currentPreviewItem) {
      initPreviewSlot();
    }
    return currentPreviewItem;
  }

  function updateItemLinks(item, url) {
    if (!item) return;
    const openBtn = item.querySelector('.video-open');
    const downloadBtn = item.querySelector('.video-download');
    const link = item.querySelector('.video-item-link');
    const safeUrl = url || '';
    item.dataset.url = safeUrl;
    if (link) {
      link.textContent = safeUrl;
      link.classList.toggle('has-url', Boolean(safeUrl));
    }
    if (openBtn) {
      if (safeUrl) {
        openBtn.href = safeUrl;
        openBtn.classList.remove('hidden');
      } else {
        openBtn.classList.add('hidden');
        openBtn.removeAttribute('href');
      }
    }
    if (downloadBtn) {
      downloadBtn.dataset.url = safeUrl;
      downloadBtn.disabled = !safeUrl;
    }
    if (safeUrl) {
      item.classList.remove('is-pending');
    }
  }

  function setIndeterminate(active) {
    if (!progressBar) return;
    if (active) {
      progressBar.classList.add('indeterminate');
    } else {
      progressBar.classList.remove('indeterminate');
    }
  }

  function startElapsedTimer() {
    stopElapsedTimer();
    if (!durationValue) return;
    elapsedTimer = setInterval(() => {
      if (!startAt) return;
      const seconds = Math.max(0, Math.round((Date.now() - startAt) / 1000));
      durationValue.textContent = `耗时 ${seconds}s`;
    }, 1000);
  }

  function stopElapsedTimer() {
    if (elapsedTimer) {
      clearInterval(elapsedTimer);
      elapsedTimer = null;
    }
  }

  function clearFileSelection() {
    fileDataUrl = '';
    if (imageFileInput) {
      imageFileInput.value = '';
    }
    if (imageFileName) {
      imageFileName.textContent = '未选择文件';
    }
  }

  function normalizeAuthHeader(authHeader) {
    if (!authHeader) return '';
    if (authHeader.startsWith('Bearer ')) {
      return authHeader.slice(7).trim();
    }
    return authHeader;
  }

  function buildSseUrl(taskId, rawPublicKey) {
    const httpProtocol = window.location.protocol === 'https:' ? 'https' : 'http';
    const base = `${httpProtocol}://${window.location.host}/v1/public/video/sse`;
    const params = new URLSearchParams();
    params.set('task_id', taskId);
    params.set('t', String(Date.now()));
    if (rawPublicKey) {
      params.set('public_key', rawPublicKey);
    }
    return `${base}?${params.toString()}`;
  }

  async function createVideoTask(authHeader) {
    const prompt = promptInput ? promptInput.value.trim() : '';
    const rawUrl = imageUrlInput ? imageUrlInput.value.trim() : '';
    if (fileDataUrl && rawUrl) {
      toast('参考图只能选择其一：URL/Base64 或 本地上传', 'error');
      throw new Error('invalid_reference');
    }
    const imageUrl = fileDataUrl || rawUrl;
    const res = await fetch('/v1/public/video/start', {
      method: 'POST',
      headers: {
        ...buildAuthHeaders(authHeader),
        'Content-Type': 'application/json'
      },
      body: JSON.stringify({
        prompt,
        image_url: imageUrl || null,
        reasoning_effort: DEFAULT_REASONING_EFFORT,
        aspect_ratio: ratioSelect ? ratioSelect.value : '3:2',
        video_length: lengthSelect ? parseInt(lengthSelect.value, 10) : 6,
        resolution_name: resolutionSelect ? resolutionSelect.value : '480p',
        preset: presetSelect ? presetSelect.value : 'normal'
      })
    });
    if (!res.ok) {
      const text = await res.text();
      throw new Error(text || 'Failed to create task');
    }
    const data = await res.json();
    return data && data.task_id ? String(data.task_id) : '';
  }

  async function stopVideoTask(taskId, authHeader) {
    if (!taskId) return;
    try {
      await fetch('/v1/public/video/stop', {
        method: 'POST',
        headers: {
          ...buildAuthHeaders(authHeader),
          'Content-Type': 'application/json'
        },
        body: JSON.stringify({ task_ids: [taskId] })
      });
    } catch (e) {
      // ignore
    }
  }

  function extractVideoInfo(buffer) {
    if (!buffer) return null;
    if (buffer.includes('<video')) {
      const matches = buffer.match(/<video[\s\S]*?<\/video>/gi);
      if (matches && matches.length) {
        return { html: matches[matches.length - 1] };
      }
    }
    const mdMatches = buffer.match(/\[video\]\(([^)]+)\)/g);
    if (mdMatches && mdMatches.length) {
      const last = mdMatches[mdMatches.length - 1];
      const urlMatch = last.match(/\[video\]\(([^)]+)\)/);
      if (urlMatch) {
        return { url: urlMatch[1] };
      }
    }
    const urlMatches = buffer.match(/https?:\/\/[^\s<)]+/g);
    if (urlMatches && urlMatches.length) {
      return { url: urlMatches[urlMatches.length - 1] };
    }
    return null;
  }

  function renderVideoFromHtml(html) {
    const container = ensurePreviewSlot();
    if (!container) return;
    const body = container.querySelector('.video-item-body');
    if (!body) return;
    body.innerHTML = html;
    const videoEl = body.querySelector('video');
    let videoUrl = '';
    if (videoEl) {
      videoEl.controls = true;
      videoEl.preload = 'metadata';
      const source = videoEl.querySelector('source');
      if (source && source.getAttribute('src')) {
        videoUrl = source.getAttribute('src');
      } else if (videoEl.getAttribute('src')) {
        videoUrl = videoEl.getAttribute('src');
      }
    }
    updateItemLinks(container, videoUrl);
  }

  function renderVideoFromUrl(url) {
    const container = ensurePreviewSlot();
    if (!container) return;
    const safeUrl = url || '';
    const body = container.querySelector('.video-item-body');
    if (!body) return;
    body.innerHTML = `\n      <video controls preload="metadata">\n        <source src="${safeUrl}" type="video/mp4">\n      </video>\n    `;
    updateItemLinks(container, safeUrl);
  }

  function handleDelta(text) {
    if (!text) return;
    if (text.includes('<think>') || text.includes('</think>')) {
      return;
    }
    if (text.includes('超分辨率')) {
      setStatus('connecting', '超分辨率中');
      setIndeterminate(true);
      if (progressText) {
        progressText.textContent = '超分辨率中';
      }
      return;
    }

    if (!collectingContent) {
      const maybeVideo = text.includes('<video') || text.includes('[video](') || text.includes('http://') || text.includes('https://');
      if (maybeVideo) {
        collectingContent = true;
      }
    }

    if (collectingContent) {
      contentBuffer += text;
      const info = extractVideoInfo(contentBuffer);
      if (info) {
        if (info.html) {
          renderVideoFromHtml(info.html);
        } else if (info.url) {
          renderVideoFromUrl(info.url);
        }
      }
      return;
    }

    progressBuffer += text;
    const matches = [...progressBuffer.matchAll(/进度\s*(\d+)%/g)];
    if (matches.length) {
      const last = matches[matches.length - 1];
      const value = parseInt(last[1], 10);
      setIndeterminate(false);
      updateProgress(value);
      progressBuffer = progressBuffer.slice(Math.max(0, progressBuffer.length - 200));
    }
  }

  function closeSource() {
    if (currentSource) {
      try {
        currentSource.close();
      } catch (e) {
        // ignore
      }
      currentSource = null;
    }
  }

  async function startConnection() {
    const prompt = promptInput ? promptInput.value.trim() : '';
    if (!prompt) {
      toast('请输入提示词', 'error');
      return;
    }

    if (isRunning) {
      toast('已在生成中', 'warning');
      return;
    }

    const authHeader = await ensurePublicKey();
    if (authHeader === null) {
      toast('请先配置 Public Key', 'error');
      window.location.href = '/login';
      return;
    }

    isRunning = true;
    startBtn.disabled = true;
    updateMeta();
    resetOutput(true);
    initPreviewSlot();
    setStatus('connecting', '连接中');

    let taskId = '';
    try {
      taskId = await createVideoTask(authHeader);
    } catch (e) {
      setStatus('error', '创建任务失败');
      startBtn.disabled = false;
      isRunning = false;
      return;
    }

    currentTaskId = taskId;
    startAt = Date.now();
    setStatus('connected', '生成中');
    setButtons(true);
    setIndeterminate(true);
    startElapsedTimer();

    const rawPublicKey = normalizeAuthHeader(authHeader);
    const url = buildSseUrl(taskId, rawPublicKey);
    closeSource();
    const es = new EventSource(url);
    currentSource = es;

    es.onopen = () => {
      setStatus('connected', '生成中');
    };

    es.onmessage = (event) => {
      if (!event || !event.data) return;
      if (event.data === '[DONE]') {
        finishRun();
        return;
      }
      let payload = null;
      try {
        payload = JSON.parse(event.data);
      } catch (e) {
        return;
      }
      if (payload && payload.error) {
        toast(payload.error, 'error');
        setStatus('error', '生成失败');
        finishRun(true);
        return;
      }
      const choice = payload.choices && payload.choices[0];
      const delta = choice && choice.delta ? choice.delta : null;
      if (delta && delta.content) {
        handleDelta(delta.content);
      }
      if (choice && choice.finish_reason === 'stop') {
        finishRun();
      }
    };

    es.onerror = () => {
      if (!isRunning) return;
      setStatus('error', '连接错误');
      finishRun(true);
    };
  }

  async function stopConnection() {
    const authHeader = await ensurePublicKey();
    if (authHeader !== null) {
      await stopVideoTask(currentTaskId, authHeader);
    }
    closeSource();
    isRunning = false;
    currentTaskId = '';
    stopElapsedTimer();
    setButtons(false);
    setStatus('', '未连接');
  }

  function finishRun(hasError) {
    if (!isRunning) return;
    closeSource();
    isRunning = false;
    setButtons(false);
    stopElapsedTimer();
    if (!hasError) {
      setStatus('connected', '完成');
      setIndeterminate(false);
      updateProgress(100);
    }
    if (durationValue && startAt) {
      const seconds = Math.max(0, Math.round((Date.now() - startAt) / 1000));
      durationValue.textContent = `耗时 ${seconds}s`;
    }
  }

  if (startBtn) {
    startBtn.addEventListener('click', () => startConnection());
  }

  if (stopBtn) {
    stopBtn.addEventListener('click', () => stopConnection());
  }

  if (clearBtn) {
    clearBtn.addEventListener('click', () => resetOutput());
  }

  if (videoStage) {
    videoStage.addEventListener('click', async (event) => {
      const target = event.target;
      if (!(target instanceof HTMLElement)) return;
      if (!target.classList.contains('video-download')) return;
      event.preventDefault();
      const item = target.closest('.video-item');
      if (!item) return;
      const url = item.dataset.url || target.dataset.url || '';
      const index = item.dataset.index || '';
      if (!url) return;
      try {
        const response = await fetch(url, { mode: 'cors' });
        if (!response.ok) {
          throw new Error('download_failed');
        }
        const blob = await response.blob();
        const blobUrl = URL.createObjectURL(blob);
        const anchor = document.createElement('a');
        anchor.href = blobUrl;
        anchor.download = index ? `grok_video_${index}.mp4` : 'grok_video.mp4';
        document.body.appendChild(anchor);
        anchor.click();
        anchor.remove();
        URL.revokeObjectURL(blobUrl);
      } catch (e) {
        toast('下载失败，请检查视频链接是否可访问', 'error');
      }
    });
  }

  if (imageFileInput) {
    imageFileInput.addEventListener('change', () => {
      const file = imageFileInput.files && imageFileInput.files[0];
      if (!file) {
        clearFileSelection();
        return;
      }
      if (imageUrlInput && imageUrlInput.value.trim()) {
        imageUrlInput.value = '';
      }
      if (imageFileName) {
        imageFileName.textContent = file.name;
      }
      const reader = new FileReader();
      reader.onload = () => {
        if (typeof reader.result === 'string') {
          fileDataUrl = reader.result;
        } else {
          fileDataUrl = '';
          toast('文件读取失败', 'error');
        }
      };
      reader.onerror = () => {
        fileDataUrl = '';
        toast('文件读取失败', 'error');
      };
      reader.readAsDataURL(file);
    });
  }

  if (selectImageFileBtn && imageFileInput) {
    selectImageFileBtn.addEventListener('click', () => {
      imageFileInput.click();
    });
  }

  if (clearImageFileBtn) {
    clearImageFileBtn.addEventListener('click', () => {
      clearFileSelection();
    });
  }

  if (imageUrlInput) {
    imageUrlInput.addEventListener('input', () => {
      if (imageUrlInput.value.trim() && fileDataUrl) {
        clearFileSelection();
      }
    });
  }

  if (promptInput) {
    promptInput.addEventListener('keydown', (event) => {
      if ((event.metaKey || event.ctrlKey) && event.key === 'Enter') {
        event.preventDefault();
        startConnection();
      }
    });
  }

  updateMeta();
})();
