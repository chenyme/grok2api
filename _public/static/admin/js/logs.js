let adminAuthHeader = '';
let logFiles = [];
let currentResponse = null;
let selectedFiles = new Set();
let autoRefreshTimer = null;
let autoRefreshIndex = -1;
let autoRefreshLongPressTimer = null;
let autoRefreshLongPressTriggered = false;
let isRefreshing = false;
let currentFileName = '';
let isMobileFilePanelCollapsed = false;

const AUTO_REFRESH_OPTIONS = [5000, 10000, 30000];
const logsById = (id) => document.getElementById(id);
const isMobileViewport = () => window.innerWidth <= 768;
const t = (key, vars = {}) => (window.I18n?.t ? I18n.t(key, vars) : key);

document.addEventListener('DOMContentLoaded', async () => {
  adminAuthHeader = await ensureAdminKey();
  if (!adminAuthHeader) return;

  bindEvents();
  syncFilePanelMode();
  if (window.I18n?.onReady) {
    I18n.onReady(() => {
      updateAutoRefreshButton();
      updateFileCountSummary();
      updateStats(currentResponse);
    });
  } else {
    updateAutoRefreshButton();
  }
  await loadFiles();
  syncAutoRefresh();
});

function bindEvents() {
  logsById('refresh-btn')?.addEventListener('click', async () => {
    await refreshCurrentView();
  });

  const autoRefreshBtn = logsById('auto-refresh-btn');
  autoRefreshBtn?.addEventListener('click', (event) => {
    if (autoRefreshLongPressTriggered) {
      autoRefreshLongPressTriggered = false;
      event.preventDefault();
      return;
    }
    cycleAutoRefresh();
  });
  autoRefreshBtn?.addEventListener('mousedown', startAutoRefreshLongPress);
  autoRefreshBtn?.addEventListener('touchstart', startAutoRefreshLongPress, { passive: true });
  autoRefreshBtn?.addEventListener('mouseup', cancelAutoRefreshLongPress);
  autoRefreshBtn?.addEventListener('mouseleave', cancelAutoRefreshLongPress);
  autoRefreshBtn?.addEventListener('touchend', cancelAutoRefreshLongPress);
  autoRefreshBtn?.addEventListener('touchcancel', cancelAutoRefreshLongPress);

  logsById('apply-btn')?.addEventListener('click', async () => {
    await loadLogs();
  });

  logsById('reset-btn')?.addEventListener('click', async () => {
    logsById('log-level').value = '';
    logsById('log-limit').value = '200';
    logsById('log-keyword').value = '';
    logsById('exclude-admin-routes').checked = true;
    updateClearLevelButton();
    await loadLogs();
  });

  logsById('clear-level-btn')?.addEventListener('click', async () => {
    logsById('log-level').value = '';
    updateClearLevelButton();
    await loadLogs();
  });

  logsById('log-keyword')?.addEventListener('keydown', async (event) => {
    if (event.key === 'Enter') {
      event.preventDefault();
      await loadLogs();
    }
  });

  logsById('select-all-files')?.addEventListener('change', (event) => {
    toggleSelectAllFiles(Boolean(event.target.checked));
  });

  logsById('delete-selected-btn')?.addEventListener('click', async () => {
    await deleteSelectedFiles();
  });

  logsById('file-panel-toggle')?.addEventListener('click', () => {
    toggleFilePanel();
  });

  window.addEventListener('resize', () => {
    syncFilePanelMode();
  });

  logsById('close-modal-btn')?.addEventListener('click', closeModal);
  logsById('log-modal')?.addEventListener('click', (event) => {
    if (event.target.id === 'log-modal') closeModal();
  });
}

async function loadFiles(keepSelection = false) {
  setLoading(true);
  try {
    const previousSelection = keepSelection ? currentFileName : '';
    const res = await fetch('/v1/admin/logs/files', {
      headers: buildAuthHeaders(adminAuthHeader),
    });
    if (!res.ok) throw new Error(await getErrorMessage(res));
    const data = await res.json();
    logFiles = Array.isArray(data.files) ? data.files : [];
    selectedFiles = new Set([...selectedFiles].filter((name) => logFiles.some((item) => item.name === name)));
    currentFileName = resolveCurrentFileName(previousSelection);
    renderFileList();
    updateFileCountSummary();
    if (currentFileName) {
      await loadLogs();
    } else {
      renderEmptyFiles();
    }
  } catch (error) {
    showToast(error.message || t('logs.loadFilesFailed'), 'error');
    renderEmptyFiles();
  } finally {
    setLoading(false);
  }
}

function resolveCurrentFileName(previousSelection) {
  if (previousSelection && logFiles.some((item) => item.name === previousSelection)) {
    return previousSelection;
  }
  return logFiles[0]?.name || '';
}

async function refreshCurrentView() {
  if (isRefreshing) return;
  isRefreshing = true;
  try {
    await loadFiles(true);
  } finally {
    isRefreshing = false;
  }
}

async function loadLogs() {
  const file = currentFileName;
  if (!file) {
    renderEntries([]);
    renderLevelBreakdown({});
    updateStats(null);
    return;
  }

  setLoading(true);
  try {
    const params = new URLSearchParams({
      file,
      limit: logsById('log-limit').value || '200',
    });
    const level = logsById('log-level').value;
    const keyword = logsById('log-keyword').value.trim();
    if (level) params.set('level', level);
    if (keyword) params.set('keyword', keyword);
    if (logsById('exclude-admin-routes').checked) {
      params.set('exclude_admin_routes', 'true');
    }

    const res = await fetch(`/v1/admin/logs?${params.toString()}`, {
      headers: buildAuthHeaders(adminAuthHeader),
    });
    if (!res.ok) throw new Error(await getErrorMessage(res));
    currentResponse = await res.json();
    renderEntries(currentResponse.entries || []);
    renderLevelBreakdown(currentResponse.stats?.levels || {});
    updateStats(currentResponse);
    updateClearLevelButton();
    syncFileListState(currentFileName);
  } catch (error) {
    renderEntries([]);
    renderLevelBreakdown({});
    updateStats(null);
    showToast(error.message || t('logs.loadFailed'), 'error');
  } finally {
    setLoading(false);
  }
}

function renderFileList() {
  const container = logsById('log-file-list');
  const count = logsById('file-count-summary');
  const selectAll = logsById('select-all-files');
  if (!container || !count || !selectAll) return;

  count.textContent = t('logs.fileCount', { count: logFiles.length });
  container.replaceChildren();

  logFiles.forEach((item) => {
    const button = document.createElement('button');
    button.type = 'button';
    button.className = `log-file-item ${item.name === currentFileName ? 'active' : ''}`;

    const row = document.createElement('div');
    row.className = 'log-file-row';

    const checkbox = document.createElement('input');
    checkbox.type = 'checkbox';
    checkbox.className = 'checkbox log-file-check';
    checkbox.checked = selectedFiles.has(item.name);

    const body = document.createElement('div');
    body.className = 'log-file-body';

    const name = document.createElement('div');
    name.className = 'log-file-name font-mono';
    name.textContent = item.name;

    const meta = document.createElement('div');
    meta.className = 'log-file-meta';
    const updated = document.createElement('span');
    updated.textContent = formatDate(item.updated_at);
    const size = document.createElement('span');
    size.textContent = formatBytes(item.size);
    meta.append(updated, size);

    body.append(name, meta);
    row.append(checkbox, body);
    button.appendChild(row);

    button.addEventListener('click', async () => {
      currentFileName = item.name;
      syncFileListState(item.name);
      if (isMobileViewport()) {
        setMobileFilePanelCollapsed(true);
      }
      await loadLogs();
    });

    checkbox.addEventListener('click', (event) => {
      event.stopPropagation();
    });
    checkbox.addEventListener('change', (event) => {
      if (event.target.checked) {
        selectedFiles.add(item.name);
      } else {
        selectedFiles.delete(item.name);
      }
      updateFileSelectionState();
    });

    container.appendChild(button);
  });

  updateFileSelectionState();
}

function renderEntries(entries) {
  const list = logsById('log-list');
  const empty = logsById('empty-state');
  if (!list || !empty) return;

  list.replaceChildren();
  empty.classList.toggle('hidden', entries.length > 0);

  entries.forEach((entry) => {
    const card = document.createElement('article');
    card.className = 'log-entry';

    const header = document.createElement('div');
    header.className = 'log-entry-header';

    const main = document.createElement('div');
    main.className = 'log-entry-main';

    const levelBtn = document.createElement('button');
    levelBtn.type = 'button';
    levelBtn.className = `log-badge ${sanitizeLevelClass(entry.level)}`;
    levelBtn.dataset.level = entry.level || '';
    levelBtn.textContent = entry.level || 'unknown';

    const time = document.createElement('div');
    time.className = 'log-entry-time font-mono';
    time.textContent = entry.time_display || '-';

    const caller = document.createElement('div');
    caller.className = 'text-xs text-[var(--accents-4)] font-mono';
    caller.textContent = entry.caller || '-';

    main.append(levelBtn, time, caller);

    const actions = document.createElement('div');
    actions.className = 'log-entry-actions';

    const copyBtn = document.createElement('button');
    copyBtn.type = 'button';
    copyBtn.className = 'geist-button-outline text-xs h-8 px-3';
    copyBtn.dataset.action = 'copy';
    copyBtn.textContent = t('common.copy');

    const rawBtn = document.createElement('button');
    rawBtn.type = 'button';
    rawBtn.className = 'geist-button-outline text-xs h-8 px-3';
    rawBtn.dataset.action = 'raw';
    rawBtn.textContent = t('common.raw');

    actions.append(copyBtn, rawBtn);
    header.append(main, actions);

    const body = document.createElement('div');
    body.className = 'log-entry-body';
    const scroll = document.createElement('div');
    scroll.className = 'log-entry-scroll';

    const message = document.createElement('div');
    message.className = 'log-message';
    message.textContent = entry.msg || '';
    scroll.appendChild(message);

    const extras = extractExtras(entry);
    if (extras.length) {
      const extrasGrid = document.createElement('div');
      extrasGrid.className = 'log-meta-grid';
      extras.forEach(([key, value]) => {
        const extraCard = document.createElement('div');
        extraCard.className = 'log-meta-card';
        const extraKey = document.createElement('div');
        extraKey.className = 'log-meta-key';
        extraKey.textContent = key;
        const extraValue = document.createElement('div');
        extraValue.className = 'log-meta-value font-mono';
        extraValue.textContent = stringifyValue(value);
        extraCard.append(extraKey, extraValue);
        extrasGrid.appendChild(extraCard);
      });
      scroll.appendChild(extrasGrid);
    }

    if (entry.stacktrace) {
      const stacktrace = document.createElement('pre');
      stacktrace.className = 'log-stacktrace font-mono';
      stacktrace.textContent = entry.stacktrace;
      scroll.appendChild(stacktrace);
    }

    body.appendChild(scroll);
    card.append(header, body);

    copyBtn.addEventListener('click', async () => {
      await copyText(entry.raw || JSON.stringify(entry, null, 2));
    });
    rawBtn.addEventListener('click', () => {
      openModal(entry);
    });
    levelBtn.addEventListener('click', async () => {
      await setLevelFilter(entry.level || '');
    });

    list.appendChild(card);
  });
}

function renderLevelBreakdown(levels) {
  const container = logsById('level-breakdown');
  if (!container) return;
  container.replaceChildren();
  const activeLevel = logsById('log-level').value;
  const names = Object.keys(levels).sort((a, b) => (levels[b] || 0) - (levels[a] || 0));

  names.forEach((level) => {
    const pill = document.createElement('button');
    pill.type = 'button';
    pill.className = `level-pill ${sanitizeLevelClass(level)} ${activeLevel === level ? 'active' : ''}`;

    const dot = document.createElement('span');
    dot.className = `level-pill-dot ${sanitizeLevelClass(level)}`;
    const label = document.createElement('span');
    label.className = 'font-mono';
    label.textContent = level;
    const count = document.createElement('strong');
    count.textContent = String(levels[level]);

    pill.append(dot, label, count);
    pill.addEventListener('click', async () => {
      await setLevelFilter(activeLevel === level ? '' : level);
    });
    container.appendChild(pill);
  });
}

async function setLevelFilter(level) {
  logsById('log-level').value = level;
  updateClearLevelButton();
  await loadLogs();
}

function updateClearLevelButton() {
  const button = logsById('clear-level-btn');
  if (!button) return;
  button.classList.toggle('hidden', !logsById('log-level').value);
}

function updateStats(response) {
  const fileName = response?.file?.name || currentFileName || '-';
  const updated = response?.file?.updated_at ? formatDate(response.file.updated_at) : '-';
  const totalSize = logFiles.reduce((sum, item) => sum + Number(item?.size || 0), 0);
  const matched = response?.stats?.matched || 0;
  const warning = response?.stats?.levels?.warning || 0;
  const error = response?.stats?.levels?.error || 0;

  logsById('stat-file').textContent = fileName;
  logsById('stat-updated').textContent = updated;
  logsById('stat-size').textContent = formatBytes(totalSize);
  logsById('stat-matched').textContent = `${matched}`;
  logsById('stat-risk').textContent = `${warning + error}`;
}

function renderEmptyFiles() {
  const list = logsById('log-file-list');
  if (list) {
    const empty = document.createElement('div');
    empty.className = 'table-empty';
    empty.textContent = t('logs.noFiles');
    list.replaceChildren(empty);
  }
  currentFileName = '';
  selectedFiles.clear();
  updateFileCountSummary();
  updateFileSelectionState();
  renderEntries([]);
  renderLevelBreakdown({});
  updateStats(null);
}

function updateFileCountSummary() {
  const summary = logsById('file-count-summary');
  if (summary) {
    summary.textContent = t('logs.fileCount', { count: logFiles.length });
  }
}

function setLoading(loading) {
  logsById('loading-state')?.classList.toggle('hidden', !loading);
}

function syncFileListState(activeName) {
  document.querySelectorAll('.log-file-item').forEach((item) => {
    const name = item.querySelector('.log-file-name')?.textContent;
    item.classList.toggle('active', name === activeName);
  });
}

function toggleSelectAllFiles(checked) {
  selectedFiles = checked ? new Set(logFiles.map((item) => item.name)) : new Set();
  renderFileList();
}

function updateFileSelectionState() {
  const selectAll = logsById('select-all-files');
  if (selectAll) {
    selectAll.checked = logFiles.length > 0 && selectedFiles.size === logFiles.length;
    selectAll.indeterminate = selectedFiles.size > 0 && selectedFiles.size < logFiles.length;
  }
  const deleteButton = logsById('delete-selected-btn');
  if (deleteButton) {
    deleteButton.disabled = selectedFiles.size === 0;
  }
}

async function deleteSelectedFiles() {
  const files = [...selectedFiles];
  if (!files.length) {
    showToast(t('logs.deleteNone'), 'error');
    return;
  }
  if (!window.confirm(t('logs.deleteConfirm', { count: files.length }))) {
    return;
  }

  try {
    const res = await fetch('/v1/admin/logs/delete', {
      method: 'POST',
      headers: {
        ...buildAuthHeaders(adminAuthHeader),
        'Content-Type': 'application/json',
      },
      body: JSON.stringify({ files }),
    });
    if (!res.ok) throw new Error(await getErrorMessage(res));
    const data = await res.json();
    selectedFiles.clear();
    const deleted = Number(data.deleted?.length || 0);
    const failed = Number(data.failed?.length || 0);
    showToast(t('logs.deleteResult', { deleted, failed }), failed > 0 ? 'info' : 'success');
    await loadFiles(true);
  } catch (error) {
    showToast(error.message || t('logs.deleteFailed'), 'error');
  }
}

function cycleAutoRefresh() {
  autoRefreshIndex += 1;
  if (autoRefreshIndex >= AUTO_REFRESH_OPTIONS.length) {
    autoRefreshIndex = -1;
  }
  syncAutoRefresh();
  updateAutoRefreshButton();
  const label = autoRefreshIndex >= 0
    ? t('logs.autoRefresh.intervalLabel', { seconds: AUTO_REFRESH_OPTIONS[autoRefreshIndex] / 1000 })
    : t('logs.autoRefresh.offShort');
  showToast(t('logs.autoRefresh.changed', { label }), 'success');
}

function syncAutoRefresh() {
  if (autoRefreshTimer) {
    clearInterval(autoRefreshTimer);
    autoRefreshTimer = null;
  }

  if (autoRefreshIndex < 0) {
    return;
  }

  const interval = AUTO_REFRESH_OPTIONS[autoRefreshIndex];
  autoRefreshTimer = window.setInterval(async () => {
    if (document.hidden) return;
    await refreshCurrentView();
  }, interval);
}

function updateAutoRefreshButton() {
  const button = logsById('auto-refresh-btn');
  if (!button) return;
  const text = autoRefreshIndex < 0
    ? t('logs.autoRefresh.off')
    : t('logs.autoRefresh.interval', { seconds: AUTO_REFRESH_OPTIONS[autoRefreshIndex] / 1000 });
  button.textContent = text;
  button.setAttribute('aria-label', t('logs.autoRefresh.buttonAria'));
  button.classList.toggle('auto-refresh-active', autoRefreshIndex >= 0);
}

function startAutoRefreshLongPress() {
  cancelAutoRefreshLongPress();
  autoRefreshLongPressTriggered = false;
  autoRefreshLongPressTimer = window.setTimeout(() => {
    if (autoRefreshIndex >= 0) {
      autoRefreshIndex = -1;
      autoRefreshLongPressTriggered = true;
      syncAutoRefresh();
      updateAutoRefreshButton();
      showToast(t('logs.autoRefresh.disabled'), 'success');
    }
    autoRefreshLongPressTimer = null;
  }, 600);
}

function cancelAutoRefreshLongPress() {
  if (autoRefreshLongPressTimer) {
    clearTimeout(autoRefreshLongPressTimer);
    autoRefreshLongPressTimer = null;
  }
}

function toggleFilePanel() {
  if (!isMobileViewport()) return;
  setMobileFilePanelCollapsed(!isMobileFilePanelCollapsed);
}

function syncFilePanelMode() {
  if (isMobileViewport()) {
    if (!logsById('file-panel-body')?.dataset.initialized) {
      logsById('file-panel-body').dataset.initialized = 'true';
      isMobileFilePanelCollapsed = true;
    }
    setMobileFilePanelCollapsed(isMobileFilePanelCollapsed);
  } else {
    isMobileFilePanelCollapsed = false;
    logsById('file-panel-body')?.classList.remove('mobile-collapsed');
    logsById('file-panel-toggle')?.classList.add('desktop-hidden');
    logsById('file-panel-toggle')?.setAttribute('aria-expanded', 'true');
  }
}

function setMobileFilePanelCollapsed(collapsed) {
  isMobileFilePanelCollapsed = collapsed;
  const body = logsById('file-panel-body');
  const toggle = logsById('file-panel-toggle');
  const text = logsById('file-panel-toggle-text');
  if (!body || !toggle || !text) return;

  if (isMobileViewport()) {
    toggle.classList.remove('desktop-hidden');
    body.classList.toggle('mobile-collapsed', collapsed);
    toggle.setAttribute('aria-expanded', String(!collapsed));
    text.textContent = collapsed ? t('logs.toggle.expand') : t('logs.toggle.collapse');
  } else {
    body.classList.remove('mobile-collapsed');
    toggle.classList.add('desktop-hidden');
    toggle.setAttribute('aria-expanded', 'true');
    text.textContent = t('logs.toggle.expand');
  }
}

function extractExtras(entry) {
  const hiddenKeys = new Set(['time', 'time_display', 'level', 'msg', 'caller', 'stacktrace', 'raw']);
  return Object.entries(entry).filter(([key, value]) => !hiddenKeys.has(key) && value !== null && value !== '');
}

function openModal(entry) {
  logsById('log-modal-meta').textContent = `${entry.time_display || '-'} · ${entry.level || 'unknown'} · ${entry.caller || '-'}`;
  logsById('log-modal-body').textContent = safePrettyJson(entry.raw);
  logsById('log-modal').classList.remove('hidden');
}

function closeModal() {
  logsById('log-modal').classList.add('hidden');
}

async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
    showToast(t('common.copied'), 'success');
  } catch (error) {
    showToast(t('common.copyFailed'), 'error');
  }
}

async function getErrorMessage(res) {
  try {
    const data = await res.json();
    return data.detail || data.message || `${res.status}`;
  } catch (error) {
    return `${res.status}`;
  }
}

function safePrettyJson(raw) {
  try {
    return JSON.stringify(JSON.parse(raw), null, 2);
  } catch (error) {
    return raw || '';
  }
}

function stringifyValue(value) {
  if (typeof value === 'string') return value;
  return JSON.stringify(value);
}

function formatDate(value) {
  if (!value) return '-';
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return date.toLocaleString();
}

function formatBytes(bytes) {
  const size = Number(bytes || 0);
  if (size < 1024) return `${size} B`;
  const units = ['KB', 'MB', 'GB', 'TB'];
  let value = size / 1024;
  let unitIndex = 0;
  while (value >= 1024 && unitIndex < units.length - 1) {
    value /= 1024;
    unitIndex += 1;
  }
  return `${value.toFixed(value >= 10 ? 0 : 1)} ${units[unitIndex]}`;
}

function sanitizeLevelClass(level) {
  const normalized = String(level || '').toLowerCase();
  return ['debug', 'info', 'warning', 'error'].includes(normalized) ? normalized : 'unknown';
}
