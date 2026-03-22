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
  const sizeSelect = document.getElementById('sizeSelect');
  const secondsInput = document.getElementById('secondsInput');
  const qualitySelect = document.getElementById('qualitySelect');
  const statusText = document.getElementById('statusText');
  const progressBar = document.getElementById('progressBar');
  const progressFill = document.getElementById('progressFill');
  const progressText = document.getElementById('progressText');
  const durationValue = document.getElementById('durationValue');
  const sizeValue = document.getElementById('sizeValue');
  const secondsValue = document.getElementById('secondsValue');
  const qualityValue = document.getElementById('qualityValue');
  const referenceValue = document.getElementById('referenceValue');
  const videoEmpty = document.getElementById('videoEmpty');
  const videoStage = document.getElementById('videoStage');

  const REQUEST_TIMEOUT = 120000;
  const MEDIA_READY_MAX_CHECKS = 30;
  const MEDIA_READY_INTERVAL = 3000;
  const DEFAULT_SIZE = '1280x720';
  const DEFAULT_SECONDS = 6;
  const DEFAULT_QUALITY = 'standard';

  let activeController = null;
  let isRunning = false;
  let fileDataUrl = '';
  let elapsedTimer = null;
  let startAt = 0;
  let currentPreviewItem = null;
  let previewCount = 0;

  function tr(key, vars) {
    return typeof t === 'function' ? t(key, vars) : key;
  }

  function toast(message, type) {
    if (typeof showToast === 'function') {
      showToast(message, type);
    }
  }

  function setStatus(state, text) {
    if (!statusText) return;
    statusText.textContent = text || tr('common.notConnected');
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

  function setIndeterminate(active) {
    if (!progressBar) return;
    progressBar.classList.toggle('indeterminate', Boolean(active));
  }

  function updateProgress(value, label) {
    const safe = Math.max(0, Math.min(100, Number(value) || 0));
    if (progressFill) {
      progressFill.style.width = `${safe}%`;
    }
    if (progressText) {
      progressText.textContent = label || `${safe}%`;
    }
  }

  function getSecondsValue() {
    const value = secondsInput ? parseInt(secondsInput.value, 10) : DEFAULT_SECONDS;
    if (!Number.isFinite(value)) return DEFAULT_SECONDS;
    return Math.min(30, Math.max(6, value));
  }

  function updateMeta() {
    if (sizeValue) {
      sizeValue.textContent = sizeSelect ? sizeSelect.value : DEFAULT_SIZE;
    }
    if (secondsValue) {
      secondsValue.textContent = `${getSecondsValue()}s`;
    }
    if (qualityValue) {
      qualityValue.textContent = qualitySelect ? qualitySelect.value : DEFAULT_QUALITY;
    }
    if (referenceValue) {
      if (fileDataUrl) {
        referenceValue.textContent = tr('video.referenceUploaded');
      } else if (imageUrlInput && imageUrlInput.value.trim()) {
        referenceValue.textContent = tr('video.referenceUrl');
      } else {
        referenceValue.textContent = tr('video.referenceNone');
      }
    }
  }

  function resetOutput(keepPreview) {
    currentPreviewItem = null;
    setIndeterminate(false);
    updateProgress(0, '0%');
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
      durationValue.textContent = tr('video.elapsedTimeNone');
    }
  }

  function createPlaceholder(message) {
    const placeholder = document.createElement('div');
    placeholder.className = 'video-item-placeholder';
    placeholder.textContent = message;
    return placeholder;
  }

  function initPreviewSlot() {
    if (!videoStage) return null;

    previewCount += 1;
    currentPreviewItem = document.createElement('div');
    currentPreviewItem.className = 'video-item is-pending';
    currentPreviewItem.dataset.index = String(previewCount);

    const header = document.createElement('div');
    header.className = 'video-item-bar';

    const title = document.createElement('div');
    title.className = 'video-item-title';
    title.textContent = tr('video.videoTitle', { n: previewCount });

    const actions = document.createElement('div');
    actions.className = 'video-item-actions';

    const openBtn = document.createElement('a');
    openBtn.className = 'geist-button-outline text-xs px-3 video-open hidden';
    openBtn.target = '_blank';
    openBtn.rel = 'noopener';
    openBtn.textContent = tr('video.open');

    const downloadBtn = document.createElement('button');
    downloadBtn.className = 'geist-button-outline text-xs px-3 video-download';
    downloadBtn.type = 'button';
    downloadBtn.textContent = tr('imagine.download');
    downloadBtn.disabled = true;

    actions.appendChild(openBtn);
    actions.appendChild(downloadBtn);
    header.appendChild(title);
    header.appendChild(actions);

    const body = document.createElement('div');
    body.className = 'video-item-body';
    body.appendChild(createPlaceholder(tr('video.generatingPlaceholder')));

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
    return currentPreviewItem;
  }

  function ensurePreviewSlot() {
    return currentPreviewItem || initPreviewSlot();
  }

  function setPreviewMessage(message) {
    const item = ensurePreviewSlot();
    if (!item) return;
    const body = item.querySelector('.video-item-body');
    if (!body) return;
    body.innerHTML = '';
    body.appendChild(createPlaceholder(message));
  }

  function updateItemLinks(item, url) {
    if (!item) return;
    const safeUrl = url || '';
    const openBtn = item.querySelector('.video-open');
    const downloadBtn = item.querySelector('.video-download');
    const link = item.querySelector('.video-item-link');

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
        openBtn.removeAttribute('href');
        openBtn.classList.add('hidden');
      }
    }
    if (downloadBtn) {
      downloadBtn.dataset.url = safeUrl;
      downloadBtn.disabled = !safeUrl;
    }
    item.classList.toggle('is-pending', !safeUrl);
  }

  function renderVideoUrl(url) {
    const item = ensurePreviewSlot();
    if (!item) return;
    const body = item.querySelector('.video-item-body');
    if (!body) return;

    body.innerHTML = '';

    const video = document.createElement('video');
    video.controls = true;
    video.preload = 'metadata';

    const source = document.createElement('source');
    source.src = url;
    source.type = 'video/mp4';

    video.appendChild(source);
    body.appendChild(video);
    updateItemLinks(item, url);
  }

  function startElapsedTimer() {
    stopElapsedTimer();
    if (!durationValue) return;
    elapsedTimer = setInterval(() => {
      if (!startAt) return;
      const seconds = Math.max(0, Math.round((Date.now() - startAt) / 1000));
      durationValue.textContent = tr('video.elapsedTime', { sec: seconds });
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
      imageFileName.textContent = tr('common.noFileSelected');
    }
    updateMeta();
  }

  function normalizeUrl(url) {
    if (!url) return '';
    const base = window.location.origin.replace(/\/+$/, '');
    if (/^https?:\/\//i.test(url)) return url;
    if (url.startsWith('/')) return `${base}${url}`;
    return `${base}/${url}`;
  }

  function sleep(ms) {
    return new Promise((resolve) => setTimeout(resolve, ms));
  }

  function timeoutSignal(ms, extraSignal) {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(new Error('timeout')), ms);

    if (extraSignal) {
      if (extraSignal.aborted) {
        controller.abort(extraSignal.reason || new Error('aborted'));
      } else {
        extraSignal.addEventListener(
          'abort',
          () => controller.abort(extraSignal.reason || new Error('aborted')),
          { once: true }
        );
      }
    }

    return {
      signal: controller.signal,
      clear() {
        clearTimeout(timer);
      }
    };
  }

  async function postJson(url, payload, signal, authHeader) {
    const timer = timeoutSignal(REQUEST_TIMEOUT, signal);
    try {
      const response = await fetch(url, {
        method: 'POST',
        headers: {
          ...buildAuthHeaders(authHeader),
          'Content-Type': 'application/json'
        },
        body: JSON.stringify(payload),
        signal: timer.signal
      });

      const text = await response.text();
      let data = {};
      try {
        data = text ? JSON.parse(text) : {};
      } catch (error) {
        data = { raw: text };
      }

      if (!response.ok) {
        const message = data?.error?.message || data?.detail || data?.raw || `${tr('common.requestFailed')} (${response.status})`;
        throw new Error(String(message));
      }

      return data;
    } finally {
      timer.clear();
    }
  }

  function extractVideoUrls(data) {
    const list = [];

    for (const key of ['url', 'video_url', 'file_url']) {
      const value = data?.[key];
      if (typeof value === 'string' && value.trim()) {
        list.push(value.trim());
      }
    }

    for (const groupKey of ['data', 'output']) {
      const group = data?.[groupKey];
      if (!Array.isArray(group)) continue;
      for (const item of group) {
        if (!item || typeof item !== 'object') continue;
        for (const key of ['url', 'video_url', 'file_url']) {
          const value = item[key];
          if (typeof value === 'string' && value.trim()) {
            list.push(value.trim());
          }
        }
      }
    }

    return [...new Set(list.map((value) => normalizeUrl(value)).filter(Boolean))];
  }

  async function checkMediaReady(url, signal) {
    const timer = timeoutSignal(30000, signal);
    try {
      const response = await fetch(url, {
        method: 'GET',
        cache: 'no-store',
        signal: timer.signal
      });
      if (!response.ok) return false;
      const contentType = (response.headers.get('content-type') || '').toLowerCase();
      return contentType.startsWith('video/') || contentType.includes('application/octet-stream');
    } catch (error) {
      return false;
    } finally {
      timer.clear();
    }
  }

  async function waitForVideoReady(url, signal) {
    for (let i = 0; i < MEDIA_READY_MAX_CHECKS; i += 1) {
      if (signal?.aborted) return null;
      updateProgress(
        Math.min(95, 60 + Math.round(((i + 1) / MEDIA_READY_MAX_CHECKS) * 35)),
        tr('video.pollingProgress', { current: i + 1, total: MEDIA_READY_MAX_CHECKS })
      );
      const ready = await checkMediaReady(url, signal);
      if (ready) {
        return url;
      }
      await sleep(MEDIA_READY_INTERVAL);
    }
    return null;
  }

  function buildPayload(prompt, referenceImage) {
    const payload = {
      model: 'grok-imagine-1.0-video',
      prompt,
      size: sizeSelect ? sizeSelect.value : DEFAULT_SIZE,
      seconds: getSecondsValue(),
      quality: qualitySelect ? qualitySelect.value : DEFAULT_QUALITY
    };

    if (referenceImage) {
      payload.image_reference = { image_url: referenceImage };
    }

    return payload;
  }

  function resolveReferenceImage() {
    const rawUrl = imageUrlInput ? imageUrlInput.value.trim() : '';
    if (fileDataUrl && rawUrl) {
      throw new Error(tr('video.referenceConflict'));
    }
    if (fileDataUrl) {
      return fileDataUrl;
    }
    if (!rawUrl) {
      return '';
    }
    if (rawUrl.startsWith('data:')) {
      return rawUrl;
    }
    return normalizeUrl(rawUrl);
  }

  async function startConnection() {
    const prompt = promptInput ? promptInput.value.trim() : '';
    if (!prompt) {
      toast(tr('common.enterPrompt'), 'error');
      return;
    }
    if (isRunning) {
      toast(tr('video.alreadyGenerating'), 'warning');
      return;
    }

    const authHeader = await ensureFunctionKey();
    if (authHeader === null) {
      toast(tr('common.configurePublicKey'), 'error');
      window.location.href = '/login';
      return;
    }

    let referenceImage = '';
    try {
      referenceImage = resolveReferenceImage();
    } catch (error) {
      const message = error instanceof Error ? error.message : tr('video.referenceConflict');
      toast(message, 'error');
      return;
    }

    if (secondsInput) {
      secondsInput.value = String(getSecondsValue());
    }
    updateMeta();
    resetOutput(true);
    initPreviewSlot();

    const controller = new AbortController();
    activeController = controller;
    isRunning = true;
    startAt = Date.now();
    startElapsedTimer();
    setButtons(true);
    setStatus('connecting', tr('video.requestingStatus'));
    setIndeterminate(false);
    updateProgress(15, tr('video.requesting'));
    setPreviewMessage(tr('video.generatingPlaceholder'));

    try {
      const payload = buildPayload(prompt, referenceImage);
      const data = await postJson('/v1/function/videos', payload, controller.signal, authHeader);

      if (controller.signal.aborted) return;

      const urls = extractVideoUrls(data);
      if (!urls.length) {
        throw new Error(tr('common.generationFailed'));
      }

      const videoUrl = urls[0];
      setStatus('connecting', tr('video.pollingStatus'));
      setPreviewMessage(tr('video.pollingMedia'));
      updateItemLinks(currentPreviewItem, videoUrl);
      const readyUrl = await waitForVideoReady(videoUrl, controller.signal);
      if (controller.signal.aborted) return;
      if (!readyUrl) {
        throw new Error(tr('common.generationFailed'));
      }

      renderVideoUrl(readyUrl);
      setIndeterminate(false);
      updateProgress(100, '100%');
      setStatus('connected', tr('common.done'));
    } catch (error) {
      if (controller.signal.aborted) {
        setPreviewMessage(tr('common.stopped'));
        setStatus('', tr('common.stopped'));
      } else {
        const message = error instanceof Error ? error.message : tr('common.generationFailed');
        setPreviewMessage(message);
        setStatus('error', tr('common.generationFailed'));
        toast(message, 'error');
      }
    } finally {
      if (activeController === controller) {
        activeController = null;
      }
      isRunning = false;
      setButtons(false);
      stopElapsedTimer();
      if (durationValue && startAt) {
        const seconds = Math.max(0, Math.round((Date.now() - startAt) / 1000));
        durationValue.textContent = tr('video.elapsedTime', { sec: seconds });
      }
      startAt = 0;
    }
  }

  function stopConnection() {
    if (activeController) {
      activeController.abort(new Error('stopped'));
      activeController = null;
    }
    isRunning = false;
    stopElapsedTimer();
    setButtons(false);
    setStatus('', tr('common.stopped'));
    setPreviewMessage(tr('common.stopped'));
  }

  if (startBtn) {
    startBtn.addEventListener('click', () => startConnection());
  }

  if (stopBtn) {
    stopBtn.addEventListener('click', () => stopConnection());
  }

  if (clearBtn) {
    clearBtn.addEventListener('click', () => resetOutput(false));
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
      } catch (error) {
        toast(tr('video.downloadFailed'), 'error');
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
          updateMeta();
        } else {
          fileDataUrl = '';
          updateMeta();
          toast(tr('common.fileReadFailed'), 'error');
        }
      };
      reader.onerror = () => {
        fileDataUrl = '';
        updateMeta();
        toast(tr('common.fileReadFailed'), 'error');
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
      updateMeta();
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

  if (sizeSelect) {
    sizeSelect.addEventListener('change', () => updateMeta());
  }

  if (secondsInput) {
    secondsInput.addEventListener('change', () => {
      secondsInput.value = String(getSecondsValue());
      updateMeta();
    });
  }

  if (qualitySelect) {
    qualitySelect.addEventListener('change', () => updateMeta());
  }

  updateMeta();
  setStatus('', tr('common.notConnected'));
  updateProgress(0, '0%');
})();
