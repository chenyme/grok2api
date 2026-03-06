let adminAuthHeader = '';
let logFiles = [];
let currentResponse = null;
let selectedFiles = new Set();
let autoRefreshTimer = null;
let autoRefreshIndex = -1;
let autoRefreshLongPressTimer = null;
let isRefreshing = false;
let currentFileName = '';
let isMobileFilePanelCollapsed = false;

const AUTO_REFRESH_OPTIONS = [5000, 10000, 30000];
const logsById = (id) => document.getElementById(id);
const isMobileViewport = () => window.innerWidth <= 768;

document.addEventListener('DOMContentLoaded', async () => {
  adminAuthHeader = await ensureAdminKey();
  if (!adminAuthHeader) return;

  bindEvents();
  syncFilePanelMode();
  updateAutoRefreshButton();
  await loadFiles();
  syncAutoRefresh();
});

function bindEvents() {
  logsById('refresh-btn')?.addEventListener('click', async () => {
    await refreshCurrentView();
  });

  const autoRefreshBtn = logsById('auto-refresh-btn');
  autoRefreshBtn?.addEventListener('click', () => {
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
    showToast(error.message || '加载日志文件失败', 'error');
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
    showToast(error.message || '加载日志失败', 'error');
  } finally {
    setLoading(false);
  }
}

function renderFileList() {
  const container = logsById('log-file-list');
  const count = logsById('file-count-summary');
  const selectAll = logsById('select-all-files');
  if (!container || !count || !selectAll) return;

  count.textContent = `${logFiles.length} 个日志文件`;
  container.innerHTML = '';

  logFiles.forEach((item) => {
    const button = document.createElement('button');
    button.type = 'button';
    button.className = `log-file-item ${item.name === currentFileName ? 'active' : ''}`;
    button.innerHTML = `
      <div class="log-file-row">
        <input type="checkbox" class="checkbox log-file-check" ${selectedFiles.has(item.name) ? 'checked' : ''}>
        <div class="log-file-body">
          <div class="log-file-name font-mono">${escapeHtml(item.name)}</div>
          <div class="log-file-meta">
            <span>${escapeHtml(formatDate(item.updated_at))}</span>
            <span>${escapeHtml(formatBytes(item.size))}</span>
          </div>
        </div>
      </div>
    `;

    button.addEventListener('click', async () => {
      currentFileName = item.name;
      syncFileListState(item.name);
      if (isMobileViewport()) {
        setMobileFilePanelCollapsed(true);
      }
      await loadLogs();
    });

    const checkbox = button.querySelector('.log-file-check');
    checkbox?.addEventListener('click', (event) => {
      event.stopPropagation();
    });
    checkbox?.addEventListener('change', (event) => {
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

  list.innerHTML = '';
  empty.classList.toggle('hidden', entries.length > 0);

  entries.forEach((entry) => {
    const card = document.createElement('article');
    card.className = 'log-entry';
    const extras = extractExtras(entry);
    const extrasHtml = extras.map(([key, value]) => `
      <div class="log-meta-card">
        <div class="log-meta-key">${escapeHtml(key)}</div>
        <div class="log-meta-value font-mono">${escapeHtml(stringifyValue(value))}</div>
      </div>
    `).join('');
    const stacktrace = entry.stacktrace
      ? `<pre class="log-stacktrace font-mono">${escapeHtml(entry.stacktrace)}</pre>`
      : '';

    card.innerHTML = `
      <div class="log-entry-header">
        <div class="log-entry-main">
          <button type="button" class="log-badge ${escapeHtml(entry.level)}" data-level="${escapeHtml(entry.level)}">${escapeHtml(entry.level || 'unknown')}</button>
          <div class="log-entry-time font-mono">${escapeHtml(entry.time_display || '-')}</div>
          <div class="text-xs text-[var(--accents-4)] font-mono">${escapeHtml(entry.caller || '-')}</div>
        </div>
        <div class="log-entry-actions">
          <button type="button" class="geist-button-outline text-xs h-8 px-3" data-action="copy">复制</button>
          <button type="button" class="geist-button-outline text-xs h-8 px-3" data-action="raw">原始</button>
        </div>
      </div>
      <div class="log-entry-body">
        <div class="log-entry-scroll">
          <div class="log-message">${escapeHtml(entry.msg || '')}</div>
          ${extrasHtml ? `<div class="log-meta-grid">${extrasHtml}</div>` : ''}
          ${stacktrace}
        </div>
      </div>
    `;

    card.querySelector('[data-action="copy"]')?.addEventListener('click', async () => {
      await copyText(entry.raw || JSON.stringify(entry, null, 2));
    });
    card.querySelector('[data-action="raw"]')?.addEventListener('click', () => {
      openModal(entry);
    });
    card.querySelector('[data-level]')?.addEventListener('click', async () => {
      await setLevelFilter(entry.level || '');
    });
    list.appendChild(card);
  });
}

function renderLevelBreakdown(levels) {
  const container = logsById('level-breakdown');
  if (!container) return;
  container.innerHTML = '';
  const activeLevel = logsById('log-level').value;
  const names = Object.keys(levels).sort((a, b) => (levels[b] || 0) - (levels[a] || 0));

  names.forEach((level) => {
    const pill = document.createElement('button');
    pill.type = 'button';
    pill.className = `level-pill ${escapeHtml(level)} ${activeLevel === level ? 'active' : ''}`;
    pill.innerHTML = `
      <span class="level-pill-dot ${escapeHtml(level)}"></span>
      <span class="font-mono">${escapeHtml(level)}</span>
      <strong>${levels[level]}</strong>
    `;
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
  const size = response?.file?.size ? formatBytes(response.file.size) : '0 B';
  const matched = response?.stats?.matched || 0;
  const warning = response?.stats?.levels?.warning || 0;
  const error = response?.stats?.levels?.error || 0;

  logsById('stat-file').textContent = fileName;
  logsById('stat-updated').textContent = updated;
  logsById('stat-size').textContent = size;
  logsById('stat-matched').textContent = `${matched}`;
  logsById('stat-risk').textContent = `${warning + error}`;
}

function renderEmptyFiles() {
  logsById('log-file-list').innerHTML = '<div class="table-empty">暂无日志文件</div>';
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
    summary.textContent = `${logFiles.length} 个日志文件`;
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
    showToast('请先选择要清理的日志文件', 'error');
    return;
  }
  if (!window.confirm(`确认清理 ${files.length} 个日志文件？`)) {
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
    showToast(`已清理 ${data.deleted?.length || 0} 个日志文件`, 'success');
    await loadFiles(true);
  } catch (error) {
    showToast(error.message || '清理日志失败', 'error');
  }
}

function cycleAutoRefresh() {
  autoRefreshIndex += 1;
  if (autoRefreshIndex >= AUTO_REFRESH_OPTIONS.length) {
    autoRefreshIndex = -1;
  }
  syncAutoRefresh();
  updateAutoRefreshButton();
  const label = autoRefreshIndex >= 0 ? `${AUTO_REFRESH_OPTIONS[autoRefreshIndex] / 1000}s` : '关闭';
  showToast(`自动刷新：${label}`, 'success');
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
  const text = autoRefreshIndex < 0 ? '自动刷新：关' : `自动刷新：${AUTO_REFRESH_OPTIONS[autoRefreshIndex] / 1000}s`;
  button.textContent = text;
  button.classList.toggle('auto-refresh-active', autoRefreshIndex >= 0);
}

function startAutoRefreshLongPress() {
  cancelAutoRefreshLongPress();
  autoRefreshLongPressTimer = window.setTimeout(() => {
    if (autoRefreshIndex >= 0) {
      autoRefreshIndex = -1;
      syncAutoRefresh();
      updateAutoRefreshButton();
      showToast('自动刷新已关闭', 'success');
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
    text.textContent = collapsed ? '展开' : '收起';
  } else {
    body.classList.remove('mobile-collapsed');
    toggle.classList.add('desktop-hidden');
    toggle.setAttribute('aria-expanded', 'true');
    text.textContent = '展开';
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
    showToast(I18n?.t('common.copied') || '已复制', 'success');
  } catch (error) {
    showToast(I18n?.t('common.copyFailed') || '复制失败', 'error');
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

function escapeHtml(value) {
  return String(value ?? '')
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}
