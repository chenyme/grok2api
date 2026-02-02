let apiKey = '';
let currentScope = 'none';
let currentToken = '';
let currentSection = 'image';
const accountMap = new Map();
const selectedTokens = new Set();
const selectedLocal = {
  image: new Set(),
  video: new Set()
};
const ui = {};
const loadFailed = new Map();
const deleteFailed = new Map();
let currentBatchAction = null;
let lastBatchAction = null;
let isLocalDeleting = false;
const cacheListState = {
  image: { loaded: false, visible: false, items: [] },
  video: { loaded: false, visible: false, items: [] }
};

async function init() {
  apiKey = await ensureApiKey();
  if (apiKey === null) return;
  cacheUI();
  setupCacheCards();
  setupConfirmDialog();
  setupFailureDialog();
  setupBatchControls();
  await loadStats();
  await showCacheSection('image');
}

function setupCacheCards() {
  if (!ui.cacheCards) return;
  ui.cacheCards.forEach(card => {
    card.addEventListener('click', () => {
      const type = card.getAttribute('data-type');
      if (type) toggleCacheList(type);
    });
  });
}

function cacheUI() {
  ui.imgCount = document.getElementById('img-count');
  ui.imgSize = document.getElementById('img-size');
  ui.videoCount = document.getElementById('video-count');
  ui.videoSize = document.getElementById('video-size');
  ui.onlineCount = document.getElementById('online-count');
  ui.onlineStatus = document.getElementById('online-status');
  ui.onlineLastClear = document.getElementById('online-last-clear');
  ui.accountTableBody = document.getElementById('account-table-body');
  ui.accountEmpty = document.getElementById('account-empty');
  ui.selectAll = document.getElementById('select-all');
  ui.localImageSelectAll = document.getElementById('local-image-select-all');
  ui.localVideoSelectAll = document.getElementById('local-video-select-all');
  ui.selectedCount = document.getElementById('selected-count');
  ui.batchActions = document.getElementById('batch-actions');
  ui.loadBtn = document.getElementById('btn-load-stats');
  ui.deleteBtn = document.getElementById('btn-delete-assets');
  ui.localCacheLists = document.getElementById('local-cache-lists');
  ui.localImageList = document.getElementById('local-image-list');
  ui.localVideoList = document.getElementById('local-video-list');
  ui.localImageBody = document.getElementById('local-image-body');
  ui.localVideoBody = document.getElementById('local-video-body');
  ui.cacheCards = document.querySelectorAll('.cache-card');
  ui.onlineAssetsTable = document.getElementById('online-assets-table');
  ui.batchProgress = document.getElementById('batch-progress');
  ui.batchProgressText = document.getElementById('batch-progress-text');
  ui.pauseActionBtn = document.getElementById('btn-pause-action');
  ui.stopActionBtn = document.getElementById('btn-stop-action');
  ui.failureDetailsBtn = document.getElementById('btn-failure-details');
  ui.confirmDialog = document.getElementById('confirm-dialog');
  ui.confirmMessage = document.getElementById('confirm-message');
  ui.confirmOk = document.getElementById('confirm-ok');
  ui.confirmCancel = document.getElementById('confirm-cancel');
  ui.failureDialog = document.getElementById('failure-dialog');
  ui.failureList = document.getElementById('failure-list');
  ui.failureClose = document.getElementById('failure-close');
  ui.failureRetry = document.getElementById('failure-retry');
}

function ensureUI() {
  if (!ui.batchActions) cacheUI();
}

let confirmResolver = null;

function setupConfirmDialog() {
  const dialog = ui.confirmDialog;
  if (!dialog) return;

  dialog.addEventListener('close', () => {
    if (!confirmResolver) return;
    const ok = dialog.returnValue === 'ok';
    confirmResolver(ok);
    confirmResolver = null;
  });

  dialog.addEventListener('cancel', (event) => {
    event.preventDefault();
    dialog.close('cancel');
  });

  dialog.addEventListener('click', (event) => {
    if (event.target === dialog) {
      dialog.close('cancel');
    }
  });

  if (ui.confirmOk) {
    ui.confirmOk.addEventListener('click', () => dialog.close('ok'));
  }
  if (ui.confirmCancel) {
    ui.confirmCancel.addEventListener('click', () => dialog.close('cancel'));
  }
}

function setupFailureDialog() {
  const dialog = ui.failureDialog;
  if (!dialog) return;
  if (ui.failureClose) {
    ui.failureClose.addEventListener('click', () => dialog.close());
  }
  if (ui.failureRetry) {
    ui.failureRetry.addEventListener('click', () => retryFailed());
  }
  dialog.addEventListener('click', (event) => {
    if (event.target === dialog) {
      dialog.close();
    }
  });
}

function setupBatchControls() {
  if (ui.pauseActionBtn) {
    ui.pauseActionBtn.addEventListener('click', () => togglePause());
  }
  if (ui.stopActionBtn) {
    ui.stopActionBtn.addEventListener('click', () => stopActiveBatch());
  }
  if (ui.failureDetailsBtn) {
    ui.failureDetailsBtn.addEventListener('click', () => showFailureDetails());
  }
}

function confirmAction(message, options = {}) {
  ensureUI();
  const dialog = ui.confirmDialog;
  if (!dialog || typeof dialog.showModal !== 'function') {
    return Promise.resolve(window.confirm(message));
  }
  if (ui.confirmMessage) ui.confirmMessage.textContent = message;
  if (ui.confirmOk) ui.confirmOk.textContent = options.okText || '确定';
  if (ui.confirmCancel) ui.confirmCancel.textContent = options.cancelText || '取消';
  return new Promise(resolve => {
    confirmResolver = resolve;
    dialog.showModal();
  });
}

function formatTime(ms) {
  if (!ms) return '';
  const dt = new Date(ms);
  return dt.toLocaleString('zh-CN', { hour12: false });
}

function calcPercent(processed, total) {
  return total ? Math.floor((processed / total) * 100) : 0;
}

const accountStates = new Map();
let isBatchLoading = false;
let isLoadPaused = false;
let batchQueue = [];
let batchTokens = [];
let batchTotal = 0;
let batchProcessed = 0;
let isBatchDeleting = false;
let isDeletePaused = false;
let deleteTotal = 0;
let deleteProcessed = 0;
const BATCH_SIZE = 10;

async function loadStats(options = {}) {
  try {
    ensureUI();
    const merge = options.merge === true;
    const silent = options.silent === true;
    const params = new URLSearchParams();
    if (options.tokens && options.tokens.length) {
      params.set('tokens', options.tokens.join(','));
      currentScope = 'selected';
    } else if (options.scope === 'all') {
      params.set('scope', 'all');
      currentScope = 'all';
    } else if (currentToken) {
      params.set('token', currentToken);
      currentScope = 'single';
    } else {
      currentScope = 'none';
    }
    const url = `/api/v1/admin/cache${params.toString() ? `?${params.toString()}` : ''}`;
    const res = await fetch(url, {
      headers: buildAuthHeaders(apiKey)
    });

    if (res.status === 401) {
      logout();
      return;
    }
    const data = await res.json();
    if (!merge) {
      accountStates.clear();
    }

    if (ui.imgCount) ui.imgCount.textContent = data.local_image.count;
    if (ui.imgSize) ui.imgSize.textContent = `${data.local_image.size_mb} MB`;
    if (ui.videoCount) ui.videoCount.textContent = data.local_video.count;
    if (ui.videoSize) ui.videoSize.textContent = `${data.local_video.size_mb} MB`;
    if (ui.onlineCount) ui.onlineCount.textContent = data.online.count;

    const statusEl = ui.onlineStatus;
    const lastClearEl = ui.onlineLastClear;
    if (data.online.status === 'ok') {
      statusEl.textContent = '连接正常';
      statusEl.className = 'text-xs text-green-600 mt-1';
    } else if (data.online.status === 'no_token') {
      statusEl.textContent = '无可用 Token';
      statusEl.className = 'text-xs text-orange-500 mt-1';
    } else if (data.online.status === 'not_loaded') {
      statusEl.textContent = '未加载';
      statusEl.className = 'text-xs text-[var(--accents-4)] mt-1';
    } else {
      statusEl.textContent = '无法连接';
      statusEl.className = 'text-xs text-red-500 mt-1';
    }

    // Update master accounts list
    updateAccountSelect(data.online_accounts || []);

    // Update dynamic states
    const details = Array.isArray(data.online_details) ? data.online_details : [];
    details.forEach(detail => {
      accountStates.set(detail.token, {
        count: detail.count,
        status: detail.status,
        last_asset_clear_at: detail.last_asset_clear_at
      });
    });
    if (data.online?.token) {
      accountStates.set(data.online.token, {
        count: data.online.count,
        status: data.online.status,
        last_asset_clear_at: data.online.last_asset_clear_at
      });
    }

    if (data.online_scope === 'all') {
      currentScope = 'all';
      currentToken = '';
    } else if (data.online_scope === 'selected') {
      currentScope = 'selected';
    } else if (data.online.token) {
      currentScope = 'single';
      currentToken = data.online.token;
    } else {
      currentScope = 'none';
    }

    if (lastClearEl) {
      const timeText = formatTime(data.online.last_asset_clear_at);
      lastClearEl.textContent = timeText ? `上次清空：${timeText}` : '';
    }

    renderAccountTable(data);
    return data;
  } catch (e) {
    if (!silent) showToast('加载统计失败', 'error');
    return null;
  }
}

function updateAccountSelect(accounts) {
  accountMap.clear();
  accounts.forEach(account => {
    accountMap.set(account.token, account);
  });
}

function renderAccountTable(data) {
  const tbody = ui.accountTableBody;
  const empty = ui.accountEmpty;
  if (!tbody || !empty) return;

  const details = Array.isArray(data.online_details) ? data.online_details : [];
  const accounts = Array.isArray(data.online_accounts) ? data.online_accounts : [];
  const detailsMap = new Map(details.map(item => [item.token, item]));
  let rows = [];

  if (accounts.length > 0) {
    rows = accounts.map(item => {
      const detail = detailsMap.get(item.token);
      const state = accountStates.get(item.token);
      let count = '-';
      let status = 'not_loaded';
      let last_asset_clear_at = item.last_asset_clear_at;

      if (detail) {
        count = detail.count;
        status = detail.status;
        last_asset_clear_at = detail.last_asset_clear_at ?? last_asset_clear_at;
      } else if (item.token === data.online?.token) {
        count = data.online.count;
        status = data.online.status;
        last_asset_clear_at = data.online.last_asset_clear_at ?? last_asset_clear_at;
      } else if (state) {
        count = state.count;
        status = state.status;
        last_asset_clear_at = state.last_asset_clear_at ?? last_asset_clear_at;
      }

      return {
        token: item.token,
        token_masked: item.token_masked,
        pool: item.pool,
        count,
        status,
        last_asset_clear_at
      };
    });
  } else if (details.length > 0) {
    rows = details.map(item => ({
      token: item.token,
      token_masked: item.token_masked,
      pool: (accountMap.get(item.token) || {}).pool || '-',
      count: item.count,
      status: item.status,
      last_asset_clear_at: item.last_asset_clear_at
    }));
  }

  if (rows.length === 0) {
    tbody.innerHTML = '';
    empty.classList.remove('hidden');
    return;
  }

  empty.classList.add('hidden');
  tbody.innerHTML = rows.map(row => {
    let statusClass = 'badge-gray';
    let statusText = '未加载';

    if (row.status === 'ok') {
      statusClass = 'badge-green';
      statusText = '正常';
    } else if (row.status === 'not_loaded') {
      statusClass = 'badge-gray';
      statusText = '未加载';
    } else {
      statusClass = 'badge-red';
      statusText = '异常';
    }

    const lastClear = formatTime(row.last_asset_clear_at) || '-';
    // Use token as identifier, but we might want index for simpler toggling if we switch to array-based state like token.js
    // For now, keep Set-based logic but update UI
    const checked = selectedTokens.has(row.token) ? 'checked' : '';

    // Shorten token for display if not already masked, though backend gives masked
    // We'll use the masked version from backend

    const rowClass = selectedTokens.has(row.token) ? 'row-selected' : '';
    const countText = row.count === '-' ? '未加载' : row.count;
    return `
      <tr class="${rowClass}">
        <td class="text-center">
          <input type="checkbox" class="checkbox" data-token="${row.token}" ${checked} onchange="toggleSelect('${row.token}', this)">
        </td>
        <td class="text-left">
             <div class="flex items-center gap-2">
                <span class="font-mono text-xs text-gray-500" title="${row.token}">${row.token_masked}</span>
             </div>
        </td>
        <td class="text-center"><span class="badge badge-gray">${row.pool || '-'}</span></td>
        <td class="text-center"><span class="badge badge-gray">${countText}</span></td>
        <td class="text-left text-xs text-gray-500">${lastClear}</td>
        <td class="text-center">
          <div class="flex items-center justify-center gap-2">
              <button class="cache-icon-button" onclick="clearOnlineCache('${row.token}')" title="清空">
                <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="3 6 5 6 21 6"></polyline><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"></path></svg>
              </button>
          </div>
        </td>
      </tr>
    `;
  }).join('');
  syncSelectAllState();
  updateSelectedCount();
  updateBatchActionsVisibility();
}

async function clearCache(type) {
  const ok = await confirmAction(`确定要清空本地${type === 'image' ? '图片' : '视频'}缓存吗？`, { okText: '清空' });
  if (!ok) return;

  try {
    const res = await fetch('/api/v1/admin/cache/clear', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        ...buildAuthHeaders(apiKey)
      },
      body: JSON.stringify({ type })
    });

    const data = await res.json();
    if (data.status === 'success') {
      showToast(`清理成功，释放 ${data.result.size_mb} MB`, 'success');
      loadStats();
    } else {
      showToast('清理失败', 'error');
    }
  } catch (e) {
    showToast('请求失败', 'error');
  }
}

function toggleSelect(token, checkbox) {
  if (checkbox && checkbox.checked) {
    selectedTokens.add(token);
  } else {
    selectedTokens.delete(token);
  }
  if (checkbox) {
    const row = checkbox.closest('tr');
    if (row) row.classList.toggle('row-selected', checkbox.checked);
  }
  syncSelectAllState();
  updateSelectedCount();
}

function toggleSelectAll(checkbox) {
  const shouldSelect = checkbox.checked;
  selectedTokens.clear();
  if (shouldSelect) {
    accountMap.forEach((_, token) => selectedTokens.add(token));
  }
  syncRowCheckboxes();
  updateSelectedCount();
}

function toggleLocalSelect(type, name, checkbox) {
  const set = selectedLocal[type];
  if (!set) return;
  if (checkbox && checkbox.checked) {
    set.add(name);
  } else {
    set.delete(name);
  }
  if (checkbox) {
    const row = checkbox.closest('tr');
    if (row) row.classList.toggle('row-selected', checkbox.checked);
  }
  syncLocalSelectAllState(type);
  updateSelectedCount();
}

function toggleLocalSelectAll(type, checkbox) {
  const set = selectedLocal[type];
  if (!set) return;
  const shouldSelect = checkbox && checkbox.checked;
  set.clear();
  if (shouldSelect) {
    const items = cacheListState[type]?.items || [];
    items.forEach(item => {
      if (item && item.name) set.add(item.name);
    });
  }
  syncLocalRowCheckboxes(type);
  updateSelectedCount();
}

function syncLocalRowCheckboxes(type) {
  const body = type === 'image' ? ui.localImageBody : ui.localVideoBody;
  if (!body) return;
  const set = selectedLocal[type];
  const checkboxes = body.querySelectorAll('input[type="checkbox"].checkbox');
  checkboxes.forEach(cb => {
    const name = cb.getAttribute('data-name');
    if (!name) return;
    cb.checked = set.has(name);
    const row = cb.closest('tr');
    if (row) row.classList.toggle('row-selected', cb.checked);
  });
  syncLocalSelectAllState(type);
}

function syncLocalSelectAllState(type) {
  const selectAll = type === 'image' ? ui.localImageSelectAll : ui.localVideoSelectAll;
  if (!selectAll) return;
  const total = cacheListState[type]?.items?.length || 0;
  const selected = selectedLocal[type]?.size || 0;
  selectAll.checked = total > 0 && selected === total;
  selectAll.indeterminate = selected > 0 && selected < total;
}

function syncRowCheckboxes() {
  const tbody = ui.accountTableBody;
  if (!tbody) return;
  const checkboxes = tbody.querySelectorAll('input[type="checkbox"].checkbox');
  checkboxes.forEach(cb => {
    const token = cb.getAttribute('data-token');
    if (!token) return;
    cb.checked = selectedTokens.has(token);
    const row = cb.closest('tr');
    if (row) row.classList.toggle('row-selected', cb.checked);
  });
}

function syncSelectAllState() {
  const selectAll = ui.selectAll;
  if (!selectAll) return;
  const total = accountMap.size;
  const selected = selectedTokens.size;
  selectAll.checked = total > 0 && selected === total;
  selectAll.indeterminate = selected > 0 && selected < total;
}

function updateSelectedCount() {
  const el = ui.selectedCount;
  const selected = getActiveSelectedSet().size;
  if (el) el.textContent = String(selected);
  setActionButtonsState();
  updateBatchActionsVisibility();
}

function updateBatchActionsVisibility() {
  const bar = ui.batchActions;
  if (!bar) return;
  bar.classList.remove('hidden');
}

function updateLoadButton() {
  const btn = ui.loadBtn;
  if (!btn) return;
  if (currentSection === 'online') {
    btn.textContent = '加载';
    btn.title = '';
  } else {
    btn.textContent = '刷新';
    btn.title = '';
  }
}

function updateDeleteButton() {
  const btn = ui.deleteBtn;
  if (!btn) return;
  if (currentSection === 'online') {
    btn.textContent = '清理';
    btn.title = '';
  } else {
    btn.textContent = '删除';
    btn.title = '';
  }
}


function setActionButtonsState() {
  const loadBtn = ui.loadBtn;
  const deleteBtn = ui.deleteBtn;
  const disabled = isBatchLoading || isBatchDeleting || isLocalDeleting;
  const noSelection = getActiveSelectedSet().size === 0;
  if (loadBtn) {
    if (currentSection === 'online') {
      loadBtn.disabled = disabled || noSelection;
    } else {
      loadBtn.disabled = disabled;
    }
  }
  if (deleteBtn) {
    if (currentSection === 'online') {
      deleteBtn.disabled = disabled || noSelection;
    } else {
      deleteBtn.disabled = disabled || noSelection;
    }
  }
}

function updateBatchProgress() {
  const container = ui.batchProgress;
  if (!container || !ui.batchProgressText) return;
  if (currentSection !== 'online') {
    container.classList.add('hidden');
    if (ui.pauseActionBtn) ui.pauseActionBtn.classList.add('hidden');
    if (ui.stopActionBtn) ui.stopActionBtn.classList.add('hidden');
    return;
  }
  if (!isBatchLoading && !isBatchDeleting) {
    container.classList.add('hidden');
    if (ui.pauseActionBtn) ui.pauseActionBtn.classList.add('hidden');
    if (ui.stopActionBtn) ui.stopActionBtn.classList.add('hidden');
    return;
  }

  const isLoading = isBatchLoading;
  const processed = isLoading ? batchProcessed : deleteProcessed;
  const total = isLoading ? batchTotal : deleteTotal;
  const percent = calcPercent(processed, total);
  ui.batchProgressText.textContent = `${percent}%`;
  container.classList.remove('hidden');

  if (ui.pauseActionBtn) {
    const paused = isLoading ? isLoadPaused : isDeletePaused;
    ui.pauseActionBtn.textContent = paused ? '继续' : '暂停';
    ui.pauseActionBtn.classList.remove('hidden');
  }
  if (ui.stopActionBtn) {
    ui.stopActionBtn.classList.remove('hidden');
  }
}

function setOnlineStatus(text, className) {
  const statusEl = ui.onlineStatus;
  if (!statusEl) return;
  statusEl.textContent = text;
  statusEl.className = className;
}

function getActiveSelectedSet() {
  if (currentSection === 'online') return selectedTokens;
  return selectedLocal[currentSection] || new Set();
}

function updateToolbarForSection() {
  updateLoadButton();
  updateDeleteButton();
  updateSelectedCount();
  updateBatchProgress();
}

function updateOnlineCountFromTokens(tokens) {
  const el = ui.onlineCount;
  if (!el) return;
  let total = 0;
  tokens.forEach(token => {
    const state = accountStates.get(token);
    if (state && typeof state.count === 'number') {
      total += state.count;
    }
  });
  el.textContent = String(total);
}

function formatSize(bytes) {
  if (bytes === 0 || bytes === null || bytes === undefined) return '-';
  const kb = 1024;
  const mb = kb * 1024;
  if (bytes >= mb) return `${(bytes / mb).toFixed(2)} MB`;
  if (bytes >= kb) return `${(bytes / kb).toFixed(1)} KB`;
  return `${bytes} B`;
}

async function showCacheSection(type) {
  ensureUI();
  currentSection = type;
  if (ui.cacheCards) {
    ui.cacheCards.forEach(card => {
      const cardType = card.getAttribute('data-type');
      card.classList.toggle('selected', cardType === type);
    });
  }
  if (type === 'image') {
    cacheListState.image.visible = true;
    cacheListState.video.visible = false;
    if (cacheListState.image.loaded) renderLocalCacheList('image', cacheListState.image.items);
    else await loadLocalCacheList('image');
    if (ui.localCacheLists) ui.localCacheLists.classList.remove('hidden');
    if (ui.localImageList) ui.localImageList.classList.remove('hidden');
    if (ui.localVideoList) ui.localVideoList.classList.add('hidden');
    if (ui.onlineAssetsTable) ui.onlineAssetsTable.classList.add('hidden');
    updateToolbarForSection();
    return;
  }
  if (type === 'video') {
    cacheListState.video.visible = true;
    cacheListState.image.visible = false;
    if (cacheListState.video.loaded) renderLocalCacheList('video', cacheListState.video.items);
    else await loadLocalCacheList('video');
    if (ui.localCacheLists) ui.localCacheLists.classList.remove('hidden');
    if (ui.localVideoList) ui.localVideoList.classList.remove('hidden');
    if (ui.localImageList) ui.localImageList.classList.add('hidden');
    if (ui.onlineAssetsTable) ui.onlineAssetsTable.classList.add('hidden');
    updateToolbarForSection();
    return;
  }
  if (type === 'online') {
    cacheListState.image.visible = false;
    cacheListState.video.visible = false;
    if (ui.localCacheLists) ui.localCacheLists.classList.add('hidden');
    if (ui.localImageList) ui.localImageList.classList.add('hidden');
    if (ui.localVideoList) ui.localVideoList.classList.add('hidden');
    if (ui.onlineAssetsTable) ui.onlineAssetsTable.classList.remove('hidden');
    updateToolbarForSection();
  }
}

async function toggleCacheList(type) {
  await showCacheSection(type);
}

async function loadLocalCacheList(type) {
  const body = type === 'image' ? ui.localImageBody : ui.localVideoBody;
  if (!body) return;
  body.innerHTML = `<tr><td colspan="5">加载中...</td></tr>`;
  try {
    const params = new URLSearchParams({ type, page: '1', page_size: '1000' });
    const res = await fetch(`/api/v1/admin/cache/list?${params.toString()}`, {
      headers: buildAuthHeaders(apiKey)
    });
    if (!res.ok) {
      body.innerHTML = `<tr><td colspan="5">加载失败</td></tr>`;
      return;
    }
    const data = await res.json();
    const items = Array.isArray(data.items) ? data.items : [];
    cacheListState[type].items = items;
    cacheListState[type].loaded = true;
    const keep = new Set(items.map(item => item.name));
    const selected = selectedLocal[type];
    Array.from(selected).forEach(name => {
      if (!keep.has(name)) selected.delete(name);
    });
    renderLocalCacheList(type, items);
  } catch (e) {
    body.innerHTML = `<tr><td colspan="5">加载失败</td></tr>`;
  }
}

function renderLocalCacheList(type, items) {
  const body = type === 'image' ? ui.localImageBody : ui.localVideoBody;
  if (!body) return;
  if (!items || items.length === 0) {
    body.innerHTML = `<tr><td colspan="5">暂无文件</td></tr>`;
    syncLocalSelectAllState(type);
    return;
  }
  const selected = selectedLocal[type];
  body.innerHTML = items.map(item => {
    const timeText = formatTime(item.mtime_ms);
    const preview = item.preview_url ? `<img src="${item.preview_url}" alt="" class="cache-preview">` : '';
    const checked = selected.has(item.name) ? 'checked' : '';
    const rowClass = selected.has(item.name) ? 'row-selected' : '';
    return `
      <tr class="${rowClass}">
        <td class="text-center">
          <input type="checkbox" class="checkbox" data-name="${item.name}" ${checked} onchange="toggleLocalSelect('${type}', '${item.name}', this)">
        </td>
        <td class="text-left">
          <div class="flex items-center gap-2">
            ${preview}
            <span class="font-mono text-xs text-gray-500">${item.name}</span>
          </div>
        </td>
        <td class="text-left">${formatSize(item.size_bytes)}</td>
        <td class="text-left text-xs text-gray-500">${timeText}</td>
        <td class="text-center">
          <div class="cache-list-actions">
            <button class="cache-icon-button" onclick="viewLocalFile('${type}', '${item.name}')" title="查看">
              <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                <path d="M1 12s4-7 11-7 11 7 11 7-4 7-11 7S1 12 1 12z"></path>
                <circle cx="12" cy="12" r="3"></circle>
              </svg>
            </button>
            <button class="cache-icon-button" onclick="deleteLocalFile('${type}', '${item.name}')" title="删除">
              <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                <polyline points="3 6 5 6 21 6"></polyline>
                <path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"></path>
              </svg>
            </button>
          </div>
        </td>
      </tr>
    `;
  }).join('');
  syncLocalSelectAllState(type);
  updateSelectedCount();
}

function viewLocalFile(type, name) {
  const safeName = encodeURIComponent(name);
  const url = type === 'image' ? `/v1/files/image/${safeName}` : `/v1/files/video/${safeName}`;
  window.open(url, '_blank');
}

async function deleteLocalFile(type, name) {
  const ok = await confirmAction(`确定要删除该文件吗？`, { okText: '删除' });
  if (!ok) return;
  const okDelete = await requestDeleteLocalFile(type, name);
  if (!okDelete) return;
  showToast('删除成功', 'success');
  const state = cacheListState[type];
  if (state && Array.isArray(state.items)) {
    state.items = state.items.filter(item => item.name !== name);
    state.loaded = true;
    selectedLocal[type]?.delete(name);
    if (state.visible) renderLocalCacheList(type, state.items);
  }
  await loadStats();
}

async function requestDeleteLocalFile(type, name) {
  try {
    const res = await fetch('/api/v1/admin/cache/item/delete', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        ...buildAuthHeaders(apiKey)
      },
      body: JSON.stringify({ type, name })
    });
    return res.ok;
  } catch (e) {
    return false;
  }
}

async function deleteSelectedLocal(type) {
  const selected = selectedLocal[type];
  const names = selected ? Array.from(selected) : [];
  if (names.length === 0) {
    showToast('未选择文件', 'info');
    return;
  }
  const ok = await confirmAction(`确定要删除选中的 ${names.length} 个文件吗？`, { okText: '删除' });
  if (!ok) return;
  isLocalDeleting = true;
  setActionButtonsState();
  let success = 0;
  let failed = 0;
  const batchSize = 10;
  for (let i = 0; i < names.length; i += batchSize) {
    const chunk = names.slice(i, i + batchSize);
    const results = await Promise.all(chunk.map(name => requestDeleteLocalFile(type, name)));
    results.forEach((ok, idx) => {
      if (ok) {
        success += 1;
      } else {
        failed += 1;
      }
    });
  }
  const state = cacheListState[type];
  if (state && Array.isArray(state.items)) {
    const toRemove = new Set(names);
    state.items = state.items.filter(item => !toRemove.has(item.name));
    state.loaded = true;
  }
  selectedLocal[type].clear();
  if (state && state.visible) renderLocalCacheList(type, state.items);
  await loadStats();
  isLocalDeleting = false;
  setActionButtonsState();
  if (failed === 0) {
    showToast(`已删除 ${success} 个文件`, 'success');
  } else {
    showToast(`删除完成：成功 ${success}，失败 ${failed}`, 'info');
  }
}

function handleLoadClick() {
  ensureUI();
  if (isBatchLoading || isBatchDeleting) {
    showToast('当前有任务进行中', 'info');
    return;
  }
  if (currentSection === 'online') {
    loadSelectedAccounts();
  } else {
    loadLocalCacheList(currentSection);
  }
}

function handleDeleteClick() {
  ensureUI();
  if (isBatchLoading || isBatchDeleting) {
    showToast('当前有任务进行中', 'info');
    return;
  }
  if (currentSection === 'online') {
    clearSelectedAccounts();
  } else {
    deleteSelectedLocal(currentSection);
  }
}

function stopBatchLoad() {
  if (!isBatchLoading) return;
  isBatchLoading = false;
  isLoadPaused = false;
  currentBatchAction = null;
  batchQueue = [];
  setOnlineStatus('已终止', 'text-xs text-[var(--accents-4)] mt-1');
  updateLoadButton();
  setActionButtonsState();
  updateBatchActionsVisibility();
  updateBatchProgress();
  showToast('已终止剩余加载请求', 'info');
}

function stopBatchDelete() {
  if (!isBatchDeleting) return;
  isBatchDeleting = false;
  isDeletePaused = false;
  currentBatchAction = null;
  batchQueue = [];
  updateDeleteButton();
  setActionButtonsState();
  updateBatchActionsVisibility();
  updateBatchProgress();
  showToast('已终止剩余清理请求', 'info');
}

function togglePause() {
  if (isBatchLoading) {
    isLoadPaused = !isLoadPaused;
    if (isLoadPaused) {
      setOnlineStatus('已暂停', 'text-xs text-[var(--accents-4)] mt-1');
    } else {
      setOnlineStatus('加载中', 'text-xs text-blue-600 mt-1');
      processBatchQueue();
    }
  } else if (isBatchDeleting) {
    isDeletePaused = !isDeletePaused;
    if (!isDeletePaused) {
      processDeleteQueue();
    }
  }
  updateBatchProgress();
}

function stopActiveBatch() {
  if (isBatchLoading) {
    stopBatchLoad();
  } else if (isBatchDeleting) {
    stopBatchDelete();
  }
}

function getMaskedToken(token) {
  const meta = accountMap.get(token);
  if (meta && meta.token_masked) return meta.token_masked;
  if (!token) return '';
  return token.length > 12 ? `${token.slice(0, 6)}...${token.slice(-4)}` : token;
}

function showFailureDetails() {
  ensureUI();
  const dialog = ui.failureDialog;
  if (!dialog || !ui.failureList) return;
  let action = currentBatchAction || lastBatchAction;
  if (!action) {
    action = deleteFailed.size > 0 ? 'delete' : 'load';
  }
  const failures = action === 'delete' ? deleteFailed : loadFailed;
  ui.failureList.innerHTML = '';
  failures.forEach((reason, token) => {
    const item = document.createElement('div');
    item.className = 'failure-item';
    const tokenEl = document.createElement('div');
    tokenEl.className = 'failure-token';
    tokenEl.textContent = getMaskedToken(token);
    const reasonEl = document.createElement('div');
    reasonEl.textContent = reason;
    item.appendChild(tokenEl);
    item.appendChild(reasonEl);
    ui.failureList.appendChild(item);
  });
  dialog.showModal();
}

function retryFailed() {
  const action = currentBatchAction || lastBatchAction || (deleteFailed.size > 0 ? 'delete' : 'load');
  const failures = action === 'delete' ? deleteFailed : loadFailed;
  const tokens = Array.from(failures.keys());
  if (tokens.length === 0) return;
  if (isBatchLoading || isBatchDeleting) {
    showToast('请等待当前任务结束', 'info');
    return;
  }
  if (ui.failureDialog) ui.failureDialog.close();
  if (action === 'delete') {
    startBatchDelete(tokens);
  } else {
    startBatchLoad(tokens);
  }
}

function startBatchLoad(tokens) {
  if (isBatchLoading) {
    showToast('正在加载中，请稍候', 'info');
    return;
  }
  if (isBatchDeleting) {
    showToast('正在清理中，请稍候', 'info');
    return;
  }
  if (!tokens || tokens.length === 0) return;
  isBatchLoading = true;
  isLoadPaused = false;
  currentBatchAction = 'load';
  lastBatchAction = 'load';
  loadFailed.clear();
  batchTokens = tokens.slice();
  batchQueue = tokens.slice();
  batchTotal = batchQueue.length;
  batchProcessed = 0;

  batchTokens.forEach(token => accountStates.delete(token));
  updateOnlineCountFromTokens(batchTokens);
  setOnlineStatus('加载中', 'text-xs text-blue-600 mt-1');
  updateLoadButton();
  setActionButtonsState();
  if (accountMap.size > 0) {
    renderAccountTable({ online_accounts: Array.from(accountMap.values()), online_details: [], online: {} });
  }
  updateBatchActionsVisibility();
  updateBatchProgress();

  processBatchQueue();
}

async function processBatchQueue() {
  if (!isBatchLoading || isLoadPaused) return;
  if (batchQueue.length === 0) {
    finishBatchLoad();
    return;
  }

  const chunk = batchQueue.splice(0, BATCH_SIZE);
  const data = await loadStats({ tokens: chunk, merge: true, silent: true });
  if (!data) {
    chunk.forEach(token => loadFailed.set(token, '请求失败'));
  } else {
    const details = Array.isArray(data.online_details) ? data.online_details : [];
    const detailMap = new Map(details.map(item => [item.token, item]));
    chunk.forEach(token => {
      const detail = detailMap.get(token);
      if (!detail) {
        loadFailed.set(token, '返回为空');
        return;
      }
      if (detail.status !== 'ok') {
        loadFailed.set(token, detail.status);
      } else {
        loadFailed.delete(token);
      }
    });
  }
  batchProcessed += chunk.length;
  updateOnlineCountFromTokens(batchTokens);
  updateLoadButton();
  setOnlineStatus('加载中', 'text-xs text-blue-600 mt-1');
  updateBatchProgress();

  setTimeout(() => {
    processBatchQueue();
  }, 300);
}

function finishBatchLoad() {
  isBatchLoading = false;
  isLoadPaused = false;
  currentBatchAction = null;
  updateOnlineCountFromTokens(batchTokens);
  const hasError = batchTokens.some(token => {
    const state = accountStates.get(token);
    return !state || (state.status && state.status !== 'ok');
  });
  if (batchTokens.length === 0) {
    setOnlineStatus('未加载', 'text-xs text-[var(--accents-4)] mt-1');
  } else if (hasError) {
    setOnlineStatus('部分异常', 'text-xs text-orange-500 mt-1');
  } else {
    setOnlineStatus('连接正常', 'text-xs text-green-600 mt-1');
  }
  updateLoadButton();
  setActionButtonsState();
  updateBatchActionsVisibility();
  updateBatchProgress();
}

async function loadSelectedAccounts() {
  if (selectedTokens.size === 0) {
    showToast('请选择要加载的账号', 'error');
    return;
  }
  startBatchLoad(Array.from(selectedTokens));
}

async function loadAllAccounts() {
  const tokens = Array.from(accountMap.keys());
  if (tokens.length === 0) {
    showToast('暂无可用账号', 'error');
    return;
  }
  startBatchLoad(tokens);
}

async function clearSelectedAccounts() {
  if (selectedTokens.size === 0) {
    showToast('请选择要清空的账号', 'error');
    return;
  }
  if (isBatchDeleting) {
    showToast('正在清理中，请稍候', 'info');
    return;
  }
  if (isBatchLoading) {
    showToast('正在加载中，请稍候', 'info');
    return;
  }
  const ok = await confirmAction(`确定要清空选中的 ${selectedTokens.size} 个账号在线资产吗？`, { okText: '清空' });
  if (!ok) return;
  startBatchDelete(Array.from(selectedTokens));
}

function startBatchDelete(tokens) {
  if (!tokens || tokens.length === 0) return;
  isBatchDeleting = true;
  isDeletePaused = false;
  currentBatchAction = 'delete';
  lastBatchAction = 'delete';
  deleteFailed.clear();
  deleteTotal = tokens.length;
  deleteProcessed = 0;
  batchQueue = tokens.slice();
  showToast('正在批量清理在线资产，请稍候...', 'info');
  updateDeleteButton();
  setActionButtonsState();
  updateBatchActionsVisibility();
  updateBatchProgress();
  processDeleteQueue();
}

async function processDeleteQueue() {
  if (!isBatchDeleting || isDeletePaused) return;
  if (batchQueue.length === 0) {
    finishBatchDelete();
    return;
  }
  const chunk = batchQueue.splice(0, BATCH_SIZE);
  const results = await clearOnlineCacheBatch(chunk);
  if (results && results.status === 'success' && results.results) {
    Object.entries(results.results).forEach(([token, result]) => {
      if (result.status !== 'success') {
        deleteFailed.set(token, result.error || '清理失败');
      } else {
        deleteFailed.delete(token);
      }
    });
  } else {
    chunk.forEach(token => deleteFailed.set(token, '请求失败'));
  }
  deleteProcessed += chunk.length;
  updateDeleteButton();
  updateBatchProgress();
  setTimeout(() => {
    processDeleteQueue();
  }, 300);
}

function finishBatchDelete() {
  isBatchDeleting = false;
  isDeletePaused = false;
  currentBatchAction = null;
  updateDeleteButton();
  setActionButtonsState();
  updateBatchActionsVisibility();
  updateBatchProgress();
  showToast('批量清理完成', 'success');
  loadStats();
}

async function clearOnlineCacheBatch(tokens = []) {
  if (!tokens || tokens.length === 0) return;
  try {
    const res = await fetch('/api/v1/admin/cache/online/clear', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        ...buildAuthHeaders(apiKey)
      },
      body: JSON.stringify({ tokens })
    });
    const data = await res.json();
    if (!res.ok || data.status !== 'success') {
      showToast('批量清理失败', 'error');
    }
    return data;
  } catch (e) {
    showToast('请求超时或失败', 'error');
    return null;
  }
}

async function clearOnlineCache(targetToken = '', skipConfirm = false) {
  const tokenToClear = targetToken || (currentScope === 'all' ? '' : currentToken);
  if (!tokenToClear) {
    showToast('请选择要清空的账号', 'error');
    return;
  }
  const meta = accountMap.get(tokenToClear);
  const label = meta ? meta.token_masked : tokenToClear;
  if (!skipConfirm) {
    const ok = await confirmAction(`确定要清空账号 ${label} 的在线资产吗？`, { okText: '清空' });
    if (!ok) return;
  }

  showToast('正在清理在线资产，请稍候...', 'info');

  try {
    const res = await fetch('/api/v1/admin/cache/online/clear', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        ...buildAuthHeaders(apiKey)
      },
      body: JSON.stringify({ token: tokenToClear })
    });

    const data = await res.json();
    if (data.status === 'success') {
      showToast(`清理完成 (成功: ${data.result.success}, 失败: ${data.result.failed})`, 'success');
    } else {
      showToast('清理失败', 'error');
    }
  } catch (e) {
    showToast('请求超时或失败', 'error');
  }
}

window.onload = init;
