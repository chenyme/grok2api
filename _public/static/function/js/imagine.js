(() => {
  const startBtn = document.getElementById('startBtn');
  const stopBtn = document.getElementById('stopBtn');
  const clearBtn = document.getElementById('clearBtn');
  const promptInput = document.getElementById('promptInput');
  const ratioSelect = document.getElementById('ratioSelect');
  const concurrentSelect = document.getElementById('concurrentSelect');
  const autoScrollToggle = document.getElementById('autoScrollToggle');
  const autoDownloadToggle = document.getElementById('autoDownloadToggle');
  const reverseInsertToggle = document.getElementById('reverseInsertToggle');
  const selectFolderBtn = document.getElementById('selectFolderBtn');
  const folderPath = document.getElementById('folderPath');
  const statusText = document.getElementById('statusText');
  const countValue = document.getElementById('countValue');
  const activeValue = document.getElementById('activeValue');
  const latencyValue = document.getElementById('latencyValue');
  const waterfall = document.getElementById('waterfall');
  const emptyState = document.getElementById('emptyState');
  const lightbox = document.getElementById('lightbox');
  const lightboxImg = document.getElementById('lightboxImg');
  const closeLightbox = document.getElementById('closeLightbox');
  const lightboxPrev = document.getElementById('lightboxPrev');
  const lightboxNext = document.getElementById('lightboxNext');
  const batchDownloadBtn = document.getElementById('batchDownloadBtn');
  const selectionToolbar = document.getElementById('selectionToolbar');
  const toggleSelectAllBtn = document.getElementById('toggleSelectAllBtn');
  const downloadSelectedBtn = document.getElementById('downloadSelectedBtn');
  const floatingActions = document.getElementById('floatingActions');

  const REQUEST_TIMEOUT = 120000;
  const MEDIA_READY_MAX_CHECKS = 30;
  const MEDIA_READY_INTERVAL = 3000;
  const SIZE_BY_RATIO = {
    '16:9': '1280x720',
    '9:16': '720x1280',
    '3:2': '1792x1024',
    '2:3': '1024x1792',
    '1:1': '1024x1024'
  };

  let isRunning = false;
  let activeController = null;
  let imageCount = 0;
  let totalLatency = 0;
  let latencyCount = 0;
  let directoryHandle = null;
  let useFileSystemAPI = false;
  let isSelectionMode = false;
  let selectedImages = new Set();
  let currentImageIndex = -1;

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

  function updateCount(value) {
    if (countValue) {
      countValue.textContent = String(value);
    }
  }

  function updateActive(value) {
    if (activeValue) {
      activeValue.textContent = String(value);
    }
  }

  function updateLatency(value) {
    if (!latencyValue) return;
    if (typeof value === 'number' && Number.isFinite(value) && value >= 0) {
      totalLatency += value;
      latencyCount += 1;
      latencyValue.textContent = `${Math.round(totalLatency / latencyCount)} ms`;
      return;
    }
    latencyValue.textContent = '-';
  }

  function resetStats() {
    imageCount = 0;
    totalLatency = 0;
    latencyCount = 0;
    updateCount(0);
    updateActive(0);
    updateLatency(null);
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

  function extractImageUrls(data) {
    const urls = [];
    const items = Array.isArray(data?.data) ? data.data : [];
    for (const item of items) {
      if (!item || typeof item !== 'object') continue;
      const value = item.url || item.image_url;
      if (typeof value === 'string' && value.trim()) {
        urls.push(normalizeUrl(value.trim()));
      }
    }
    return [...new Set(urls)];
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
      return contentType.startsWith('image/');
    } catch (error) {
      return false;
    } finally {
      timer.clear();
    }
  }

  async function waitForImagesReady(urls, signal) {
    for (let i = 0; i < MEDIA_READY_MAX_CHECKS; i += 1) {
      if (signal?.aborted) return null;
      const results = await Promise.all(urls.map((url) => checkMediaReady(url, signal)));
      if (results.every(Boolean)) {
        return urls;
      }
      await sleep(MEDIA_READY_INTERVAL);
    }
    return null;
  }

  function getExtensionFromType(contentType, fallback = 'png') {
    const lower = (contentType || '').toLowerCase();
    if (lower.includes('png')) return 'png';
    if (lower.includes('webp')) return 'webp';
    if (lower.includes('gif')) return 'gif';
    if (lower.includes('jpeg') || lower.includes('jpg')) return 'jpg';
    return fallback;
  }

  async function fetchMediaBlob(url) {
    const response = await fetch(url, { cache: 'no-store' });
    if (!response.ok) {
      throw new Error('download_failed');
    }
    const blob = await response.blob();
    const extension = getExtensionFromType(blob.type);
    return { blob, extension };
  }

  async function saveUrlToFileSystem(url, filename) {
    if (!directoryHandle) return false;
    const { blob } = await fetchMediaBlob(url);
    const fileHandle = await directoryHandle.getFileHandle(filename, { create: true });
    const writable = await fileHandle.createWritable();
    await writable.write(blob);
    await writable.close();
    return true;
  }

  async function downloadImageUrl(url, filename) {
    try {
      const { blob } = await fetchMediaBlob(url);
      const blobUrl = URL.createObjectURL(blob);
      const link = document.createElement('a');
      link.href = blobUrl;
      link.download = filename;
      document.body.appendChild(link);
      link.click();
      link.remove();
      URL.revokeObjectURL(blobUrl);
    } catch (error) {
      const link = document.createElement('a');
      link.href = url;
      link.download = filename;
      link.target = '_blank';
      link.rel = 'noopener';
      document.body.appendChild(link);
      link.click();
      link.remove();
    }
  }

  async function saveOrDownloadImage(url, filename) {
    if (useFileSystemAPI && directoryHandle) {
      try {
        await saveUrlToFileSystem(url, filename);
        return;
      } catch (error) {
        // fallback below
      }
    }
    await downloadImageUrl(url, filename);
  }

  function buildFilename(sequence) {
    const safeSequence = sequence || imageCount + 1;
    return `imagine_${Date.now()}_${safeSequence}.png`;
  }

  function appendImageUrl(url, meta = {}) {
    if (!waterfall || !url) return;

    if (emptyState) {
      emptyState.style.display = 'none';
    }

    imageCount += 1;
    updateCount(imageCount);

    const item = document.createElement('div');
    item.className = 'waterfall-item';

    const checkbox = document.createElement('div');
    checkbox.className = 'image-checkbox';

    const img = document.createElement('img');
    img.loading = 'lazy';
    img.decoding = 'async';
    img.alt = meta.prompt || 'image';
    img.src = url;

    const metaBar = document.createElement('div');
    metaBar.className = 'waterfall-meta';

    const left = document.createElement('div');
    left.textContent = `#${meta.sequence || imageCount}`;

    const rightWrap = document.createElement('div');
    rightWrap.className = 'meta-right';

    const status = document.createElement('span');
    status.className = 'image-status done';
    status.textContent = tr('common.done');

    const right = document.createElement('span');
    if (meta.elapsedMs) {
      right.textContent = `${meta.elapsedMs}ms`;
    } else {
      right.textContent = '';
    }

    rightWrap.appendChild(status);
    rightWrap.appendChild(right);
    metaBar.appendChild(left);
    metaBar.appendChild(rightWrap);

    item.appendChild(checkbox);
    item.appendChild(img);
    item.appendChild(metaBar);

    item.dataset.imageUrl = url;
    item.dataset.prompt = meta.prompt || (promptInput ? promptInput.value.trim() : 'image');
    item.dataset.sequence = String(meta.sequence || imageCount);

    if (isSelectionMode) {
      item.classList.add('selection-mode');
    }

    if (reverseInsertToggle && reverseInsertToggle.checked) {
      waterfall.prepend(item);
    } else {
      waterfall.appendChild(item);
    }

    if (autoScrollToggle && autoScrollToggle.checked) {
      if (reverseInsertToggle && reverseInsertToggle.checked) {
        window.scrollTo({ top: 0, behavior: 'smooth' });
      } else {
        window.scrollTo({ top: document.body.scrollHeight, behavior: 'smooth' });
      }
    }

    if (autoDownloadToggle && autoDownloadToggle.checked) {
      const filename = buildFilename(meta.sequence || imageCount);
      saveOrDownloadImage(url, filename).catch(() => {
        toast(tr('common.requestFailed'), 'error');
      });
    }
  }

  function clearImages() {
    if (waterfall) {
      waterfall.innerHTML = '';
    }
    if (emptyState) {
      emptyState.style.display = 'block';
    }
    resetStats();
    exitSelectionMode();
  }

  function enterSelectionMode() {
    if (!selectionToolbar) return;
    isSelectionMode = true;
    selectedImages.clear();
    selectionToolbar.classList.remove('hidden');
    const items = document.querySelectorAll('.waterfall-item');
    items.forEach((item) => item.classList.add('selection-mode'));
    updateSelectedCount();
  }

  function exitSelectionMode() {
    if (!selectionToolbar) return;
    isSelectionMode = false;
    selectedImages.clear();
    selectionToolbar.classList.add('hidden');
    const items = document.querySelectorAll('.waterfall-item');
    items.forEach((item) => item.classList.remove('selection-mode', 'selected'));
    updateSelectedCount();
  }

  function toggleSelectionMode() {
    if (isSelectionMode) {
      exitSelectionMode();
    } else {
      enterSelectionMode();
    }
  }

  function toggleImageSelection(item) {
    if (!isSelectionMode || !item) return;
    if (item.classList.contains('selected')) {
      item.classList.remove('selected');
      selectedImages.delete(item);
    } else {
      item.classList.add('selected');
      selectedImages.add(item);
    }
    updateSelectedCount();
  }

  function updateSelectedCount() {
    const countSpan = document.getElementById('selectedCount');
    if (countSpan) {
      countSpan.textContent = String(selectedImages.size);
    }
    if (downloadSelectedBtn) {
      downloadSelectedBtn.disabled = selectedImages.size === 0;
    }
    if (toggleSelectAllBtn) {
      const items = document.querySelectorAll('.waterfall-item');
      const allSelected = items.length > 0 && selectedImages.size === items.length;
      toggleSelectAllBtn.textContent = allSelected ? tr('imagine.deselectAll') : tr('imagine.selectAll');
    }
  }

  function toggleSelectAll() {
    const items = document.querySelectorAll('.waterfall-item');
    const allSelected = items.length > 0 && selectedImages.size === items.length;
    if (allSelected) {
      items.forEach((item) => item.classList.remove('selected'));
      selectedImages.clear();
    } else {
      items.forEach((item) => {
        item.classList.add('selected');
        selectedImages.add(item);
      });
    }
    updateSelectedCount();
  }

  async function downloadSelectedImages() {
    if (selectedImages.size === 0) {
      toast(tr('imagine.noImagesSelected'), 'warning');
      return;
    }
    if (typeof JSZip === 'undefined') {
      toast(tr('imagine.jszipFailed'), 'error');
      return;
    }

    downloadSelectedBtn.disabled = true;
    downloadSelectedBtn.textContent = tr('imagine.packingBtn');

    const zip = new JSZip();
    const folder = zip.folder('images');
    let processed = 0;

    try {
      for (const item of selectedImages) {
        const url = item.dataset.imageUrl;
        const prompt = item.dataset.prompt || 'image';
        if (!url) continue;
        try {
          const { blob, extension } = await fetchMediaBlob(url);
          const safePrompt = prompt.slice(0, 30).replace(/[^a-zA-Z0-9\u4e00-\u9fa5]/g, '_') || 'image';
          folder.file(`${safePrompt}_${processed + 1}.${extension}`, blob);
          processed += 1;
          downloadSelectedBtn.textContent = tr('imagine.packingProgress', { done: processed, total: selectedImages.size });
        } catch (error) {
          // skip failed image
        }
      }

      if (processed === 0) {
        toast(tr('imagine.noImagesDownloaded'), 'error');
        return;
      }

      downloadSelectedBtn.textContent = tr('imagine.generatingZip');
      const content = await zip.generateAsync({ type: 'blob' });
      const link = document.createElement('a');
      link.href = URL.createObjectURL(content);
      link.download = `imagine_${new Date().toISOString().slice(0, 10)}_${Date.now()}.zip`;
      document.body.appendChild(link);
      link.click();
      link.remove();
      URL.revokeObjectURL(link.href);
      toast(tr('imagine.packSuccess', { count: processed }), 'success');
      exitSelectionMode();
    } catch (error) {
      toast(tr('imagine.packFailed'), 'error');
    } finally {
      if (downloadSelectedBtn) {
        downloadSelectedBtn.innerHTML = `${tr('imagine.download')} <span id="selectedCount" class="selected-count">${selectedImages.size}</span>`;
      }
      updateSelectedCount();
    }
  }

  function getAllImages() {
    return Array.from(document.querySelectorAll('.waterfall-item img'));
  }

  function updateLightbox(index) {
    const images = getAllImages();
    if (index < 0 || index >= images.length || !lightboxImg) return;
    currentImageIndex = index;
    lightboxImg.src = images[index].src;
    if (lightboxPrev) lightboxPrev.disabled = index === 0;
    if (lightboxNext) lightboxNext.disabled = index === images.length - 1;
  }

  function showPrevImage() {
    if (currentImageIndex > 0) {
      updateLightbox(currentImageIndex - 1);
    }
  }

  function showNextImage() {
    const images = getAllImages();
    if (currentImageIndex >= 0 && currentImageIndex < images.length - 1) {
      updateLightbox(currentImageIndex + 1);
    }
  }

  async function startConnection() {
    const prompt = promptInput ? promptInput.value.trim() : '';
    if (!prompt) {
      toast(tr('common.enterPrompt'), 'error');
      return;
    }
    if (isRunning) {
      toast(tr('common.alreadyRunning'), 'warning');
      return;
    }

    const authHeader = await ensureFunctionKey();
    if (authHeader === null) {
      toast(tr('common.configurePublicKey'), 'error');
      window.location.href = '/login';
      return;
    }

    clearImages();
    isRunning = true;
    setButtons(true);
    setStatus('connecting', tr('common.connecting'));
    updateActive(1);

    const controller = new AbortController();
    activeController = controller;
    const startedAt = Date.now();
    const n = concurrentSelect ? parseInt(concurrentSelect.value, 10) || 1 : 1;
    const ratio = ratioSelect ? ratioSelect.value : '2:3';
    const size = SIZE_BY_RATIO[ratio] || '1024x1792';

    try {
      const data = await postJson(
        '/v1/function/images/generations',
        {
          model: 'grok-imagine-1.0',
          prompt,
          n,
          size,
          response_format: 'url',
          stream: false
        },
        controller.signal,
        authHeader
      );

      if (controller.signal.aborted) return;

      const urls = extractImageUrls(data);
      if (!urls.length) {
        throw new Error(tr('common.generationFailed'));
      }

      setStatus('connecting', tr('common.generating'));
      const readyUrls = await waitForImagesReady(urls, controller.signal);
      if (controller.signal.aborted) return;
      if (!readyUrls) {
        throw new Error(tr('common.generationFailed'));
      }

      const elapsedMs = Date.now() - startedAt;
      readyUrls.forEach((url, index) => {
        appendImageUrl(url, {
          sequence: index + 1,
          prompt,
          elapsedMs
        });
      });

      updateLatency(elapsedMs);
      setStatus('connected', tr('common.done'));
    } catch (error) {
      if (controller.signal.aborted) {
        setStatus('', tr('common.stopped'));
      } else {
        const message = error instanceof Error ? error.message : tr('common.generationFailed');
        setStatus('error', tr('common.generationFailed'));
        toast(message, 'error');
      }
    } finally {
      if (activeController === controller) {
        activeController = null;
      }
      isRunning = false;
      updateActive(0);
      setButtons(false);
    }
  }

  function stopConnection() {
    if (activeController) {
      activeController.abort(new Error('stopped'));
      activeController = null;
    }
    isRunning = false;
    updateActive(0);
    setButtons(false);
    setStatus('', tr('common.stopped'));
  }

  if (startBtn) {
    startBtn.addEventListener('click', () => startConnection());
  }

  if (stopBtn) {
    stopBtn.addEventListener('click', () => stopConnection());
  }

  if (clearBtn) {
    clearBtn.addEventListener('click', () => clearImages());
  }

  if (promptInput) {
    promptInput.addEventListener('keydown', (event) => {
      if ((event.metaKey || event.ctrlKey) && event.key === 'Enter') {
        event.preventDefault();
        startConnection();
      }
    });
  }

  if ('showDirectoryPicker' in window && selectFolderBtn) {
    selectFolderBtn.disabled = !(autoDownloadToggle && autoDownloadToggle.checked);
    selectFolderBtn.addEventListener('click', async () => {
      try {
        directoryHandle = await window.showDirectoryPicker({ mode: 'readwrite' });
        useFileSystemAPI = true;
        if (folderPath) {
          folderPath.textContent = directoryHandle.name;
          selectFolderBtn.style.color = '#059669';
        }
        toast(tr('imagine.selectFolder', { name: directoryHandle.name }), 'success');
      } catch (error) {
        if (error?.name !== 'AbortError') {
          toast(tr('imagine.selectFolderFailed'), 'error');
        }
      }
    });
  }

  if (autoDownloadToggle && selectFolderBtn) {
    autoDownloadToggle.addEventListener('change', () => {
      if ('showDirectoryPicker' in window) {
        selectFolderBtn.disabled = !autoDownloadToggle.checked;
      }
    });
  }

  if (batchDownloadBtn) {
    batchDownloadBtn.addEventListener('click', () => toggleSelectionMode());
  }

  if (toggleSelectAllBtn) {
    toggleSelectAllBtn.addEventListener('click', () => toggleSelectAll());
  }

  if (downloadSelectedBtn) {
    downloadSelectedBtn.addEventListener('click', () => downloadSelectedImages());
  }

  if (waterfall) {
    waterfall.addEventListener('click', (event) => {
      const target = event.target;
      const item = target instanceof HTMLElement ? target.closest('.waterfall-item') : null;
      if (!item) return;

      if (isSelectionMode) {
        toggleImageSelection(item);
        return;
      }

      const img = target instanceof HTMLElement ? target.closest('.waterfall-item img') : null;
      if (!img || !lightbox) return;
      const images = getAllImages();
      const index = images.indexOf(img);
      if (index !== -1) {
        updateLightbox(index);
        lightbox.classList.add('active');
      }
    });
  }

  if (lightbox && closeLightbox) {
    closeLightbox.addEventListener('click', (event) => {
      event.stopPropagation();
      lightbox.classList.remove('active');
      currentImageIndex = -1;
    });

    lightbox.addEventListener('click', () => {
      lightbox.classList.remove('active');
      currentImageIndex = -1;
    });

    if (lightboxImg) {
      lightboxImg.addEventListener('click', (event) => event.stopPropagation());
    }

    if (lightboxPrev) {
      lightboxPrev.addEventListener('click', (event) => {
        event.stopPropagation();
        showPrevImage();
      });
    }

    if (lightboxNext) {
      lightboxNext.addEventListener('click', (event) => {
        event.stopPropagation();
        showNextImage();
      });
    }

    document.addEventListener('keydown', (event) => {
      if (!lightbox.classList.contains('active')) return;
      if (event.key === 'Escape') {
        lightbox.classList.remove('active');
        currentImageIndex = -1;
      } else if (event.key === 'ArrowLeft') {
        showPrevImage();
      } else if (event.key === 'ArrowRight') {
        showNextImage();
      }
    });
  }

  if (floatingActions) {
    let isDragging = false;
    let startX = 0;
    let startY = 0;
    let initialLeft = 0;
    let initialTop = 0;

    floatingActions.style.touchAction = 'none';

    floatingActions.addEventListener('pointerdown', (event) => {
      if (event.target instanceof HTMLElement && (event.target.tagName.toLowerCase() === 'button' || event.target.closest('button'))) {
        return;
      }

      event.preventDefault();
      isDragging = true;
      floatingActions.setPointerCapture(event.pointerId);
      startX = event.clientX;
      startY = event.clientY;

      const rect = floatingActions.getBoundingClientRect();
      if (!floatingActions.style.left) {
        floatingActions.style.left = `${rect.left}px`;
        floatingActions.style.top = `${rect.top}px`;
        floatingActions.style.transform = 'none';
        floatingActions.style.bottom = 'auto';
      }

      initialLeft = parseFloat(floatingActions.style.left);
      initialTop = parseFloat(floatingActions.style.top);
      floatingActions.classList.add('shadow-xl');
    });

    document.addEventListener('pointermove', (event) => {
      if (!isDragging) return;
      const dx = event.clientX - startX;
      const dy = event.clientY - startY;
      floatingActions.style.left = `${initialLeft + dx}px`;
      floatingActions.style.top = `${initialTop + dy}px`;
    });

    document.addEventListener('pointerup', (event) => {
      if (!isDragging) return;
      isDragging = false;
      floatingActions.releasePointerCapture(event.pointerId);
      floatingActions.classList.remove('shadow-xl');
    });
  }

  resetStats();
  setStatus('', tr('common.notConnected'));
})();
