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
const byId = (id) => document.getElementById(id);
const loadFailed = new Map();
const deleteFailed = new Map();
let currentBatchAction = null;
let lastBatchAction = null;
let isLocalDeleting = false;
const cacheListState = {
  image: { loaded: false, visible: false, items: [] },
  video: { loaded: false, visible: false, items: [] }
};
const UI_MAP = {
  imgCount: 'img-count',
  imgSize: 'img-size',
  videoCount: 'video-count',
  videoSize: 'video-size',
  onlineCount: 'online-count',
  onlineStatus: 'online-status',
  onlineLastClear: 'online-last-clear',
  accountTableBody: 'account-table-body',
  accountEmpty: 'account-empty',
  selectAll: 'select-all',
  localImageSelectAll: 'local-image-select-all',
  localVideoSelectAll: 'local-video-select-all',
  selectedCount: 'selected-count',
  batchActions: 'batch-actions',
  loadBtn: 'btn-load-stats',
  deleteBtn: 'btn-delete-assets',
  localCacheLists: 'local-cache-lists',
  localImageList: 'local-image-list',
  localVideoList: 'local-video-list',
  localImageBody: 'local-image-body',
  localVideoBody: 'local-video-body',
  onlineAssetsTable: 'online-assets-table',
  batchProgress: 'batch-progress',
  batchProgressText: 'batch-progress-text',
  pauseActionBtn: 'btn-pause-action',
  stopActionBtn: 'btn-stop-action',
  failureDetailsBtn: 'btn-failure-details',
  confirmDialog: 'confirm-dialog',
  confirmMessage: 'confirm-message',
  confirmOk: 'confirm-ok',
  confirmCancel: 'confirm-cancel',
  failureDialog: 'failure-dialog',
  failureList: 'failure-list',
  failureClose: 'failure-close',
  failureRetry: 'failure-retry'
};

function setText(el, text) {
  if (el) el.textContent = text;
}

function resolveOnlineStatus(status) {
  if (status === 'ok') {
    return { text: '连接正常', className: 'text-xs text-green-600 mt-1' };
  }
  if (status === 'no_token') {
    return { text: '无可用 Token', className: 'text-xs text-orange-500 mt-1' };
  }
  if (status === 'not_loaded') {
    return { text: '未加载', className: 'text-xs text-[var(--accents-4)] mt-1' };
  }
  return { text: '无法连接', className: 'text-xs text-red-500 mt-1' };
}

function createIconButton(title, svg, onClick) {
  const btn = document.createElement('button');
  btn.className = 'cache-icon-button';
  btn.title = title;
  btn.innerHTML = svg;
  btn.addEventListener('click', onClick);
  return btn;
}

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
  Object.entries(UI_MAP).forEach(([key, id]) => {
    ui[key] = byId(id);
  });
  ui.cacheCards = document.querySelectorAll('.cache-card');
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
let currentBatchTaskId = null;
let batchEventSource = null;

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
    applyStatsData(data, merge);
    return data;
  } catch (e) {
    if (!silent) showToast('加载统计失败', 'error');
    return null;
  }
}

function applyStatsData(data, merge = false) {
  if (!merge) {
    accountStates.clear();
  }

  setText(ui.imgCount, data.local_image.count);
  setText(ui.imgSize, `${data.local_image.size_mb} MB`);
  setText(ui.videoCount, data.local_video.count);
  setText(ui.videoSize, `${data.local_video.size_mb} MB`);
  setText(ui.onlineCount, data.online.count);

  const online = data.online || {};
  const status = resolveOnlineStatus(online.status);
  setOnlineStatus(status.text, status.className);

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
  if (online?.token) {
    accountStates.set(online.token, {
      count: online.count,
      status: online.status,
      last_asset_clear_at: online.last_asset_clear_at
    });
  }

  if (data.online_scope === 'all') {
    currentScope = 'all';
    currentToken = '';
  } else if (data.online_scope === 'selected') {
    currentScope = 'selected';
  } else if (online.token) {
    currentScope = 'single';
    currentToken = online.token;
  } else {
    currentScope = 'none';
  }

  const timeText = formatTime(online.last_asset_clear_at);
  setText(ui.onlineLastClear, timeText ? `上次清空：${timeText}` : '');

  renderAccountTable(data);
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
    tbody.replaceChildren();
    empty.classList.remove('hidden');
    return;
  }

  empty.classList.add('hidden');
  const selected = selectedTokens;
  const fragment = document.createDocumentFragment();
  rows.forEach(row => {
    const tr = document.createElement('tr');
    const isSelected = selected.has(row.token);
    if (isSelected) tr.classList.add('row-selected');

    const tdCheck = document.createElement('td');
    tdCheck.className = 'text-center';
    const checkbox = document.createElement('input');
    checkbox.type = 'checkbox';
    checkbox.className = 'checkbox';
    checkbox.checked = isSelected;
    checkbox.setAttribute('data-token', row.token);
    checkbox.addEventListener('change', () => toggleSelect(row.token, checkbox));
    tdCheck.appendChild(checkbox);

    const tdToken = document.createElement('td');
    tdToken.className = 'text-left';
    const tokenWrap = document.createElement('div');
    tokenWrap.className = 'flex items-center gap-2';
    const tokenText = document.createElement('span');
    tokenText.className = 'font-mono text-xs text-gray-500';
    tokenText.title = row.token;
    tokenText.textContent = row.token_masked || row.token;
    tokenWrap.appendChild(tokenText);
    tdToken.appendChild(tokenWrap);

    const tdPool = document.createElement('td');
    tdPool.className = 'text-center';
    const poolBadge = document.createElement('span');
    poolBadge.className = 'badge badge-gray';
    poolBadge.textContent = row.pool || '-';
    tdPool.appendChild(poolBadge);

    const tdCount = document.createElement('td');
    tdCount.className = 'text-center';
    const countBadge = document.createElement('span');
    countBadge.className = 'badge badge-gray';
    countBadge.textContent = row.count === '-' ? '未加载' : row.count;
    tdCount.appendChild(countBadge);

    const tdLast = document.createElement('td');
    tdLast.className = 'text-left text-xs text-gray-500';
    tdLast.textContent = formatTime(row.last_asset_clear_at) || '-';

    const tdActions = document.createElement('td');
    tdActions.className = 'text-center';
    const actionsWrap = document.createElement('div');
    actionsWrap.className = 'flex items-center justify-center gap-2';
    actionsWrap.appendChild(createIconButton(
      '清空',
      `<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="3 6 5 6 21 6"></polyline><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"></path></svg>`,
      () => clearOnlineCache(row.token)
    ));
    tdActions.appendChild(actionsWrap);

    tr.appendChild(tdCheck);
    tr.appendChild(tdToken);
    tr.appendChild(tdPool);
    tr.appendChild(tdCount);
    tr.appendChild(tdLast);
    tr.appendChild(tdActions);
    fragment.appendChild(tr);
  });
  tbody.replaceChildren(fragment);
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
      const state = cacheListState[type];
      if (state) {
        state.items = [];
        state.loaded = true;
      }
      if (selectedLocal[type]) selectedLocal[type].clear();
      if (state && state.visible) {
        renderLocalCacheList(type, []);
      } else {
        syncLocalSelectAllState(type);
        updateSelectedCount();
      }
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
    ui.pauseActionBtn.classList.add('hidden');
  }
  if (ui.stopActionBtn) {
    ui.stopActionBtn.classList.remove('hidden');
  }
}

function refreshBatchUI() {
  setActionButtonsState();
  updateBatchActionsVisibility();
  updateBatchProgress();
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
  let total = 0;
  tokens.forEach(token => {
    const state = accountStates.get(token);
    if (state && typeof state.count === 'number') {
      total += state.count;
    }
  });
  setText(ui.onlineCount, String(total));
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
    body.innerHTML = `<tr><td colspan="5" class="table-empty">暂无文件</td></tr>`;
    syncLocalSelectAllState(type);
    return;
  }
  const selected = selectedLocal[type];
  const fragment = document.createDocumentFragment();
  items.forEach(item => {
    const tr = document.createElement('tr');
    const isSelected = selected.has(item.name);
    if (isSelected) tr.classList.add('row-selected');

    const tdCheck = document.createElement('td');
    tdCheck.className = 'text-center';
    const checkbox = document.createElement('input');
    checkbox.type = 'checkbox';
    checkbox.className = 'checkbox';
    checkbox.checked = isSelected;
    checkbox.setAttribute('data-name', item.name);
    checkbox.onchange = () => toggleLocalSelect(type, item.name, checkbox);
    tdCheck.appendChild(checkbox);

    const tdName = document.createElement('td');
    tdName.className = 'text-left';
    const nameWrap = document.createElement('div');
    nameWrap.className = 'flex items-center gap-2';
    if (item.preview_url) {
      const img = document.createElement('img');
      img.src = item.preview_url;
      img.alt = '';
      img.className = 'cache-preview';
      nameWrap.appendChild(img);
    }
    const nameText = document.createElement('span');
    nameText.className = 'font-mono text-xs text-gray-500';
    nameText.textContent = item.name;
    nameWrap.appendChild(nameText);
    tdName.appendChild(nameWrap);

    const tdSize = document.createElement('td');
    tdSize.className = 'text-left';
    tdSize.textContent = formatSize(item.size_bytes);

    const tdTime = document.createElement('td');
    tdTime.className = 'text-left text-xs text-gray-500';
    tdTime.textContent = formatTime(item.mtime_ms);

    const tdActions = document.createElement('td');
    tdActions.className = 'text-center';
    tdActions.innerHTML = `
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
    `;

    tr.appendChild(tdCheck);
    tr.appendChild(tdName);
    tr.appendChild(tdSize);
    tr.appendChild(tdTime);
    tr.appendChild(tdActions);
    fragment.appendChild(tr);
  });
  body.replaceChildren(fragment);
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

function stopBatchLoad(options = {}) {
  if (!isBatchLoading) return;
  isBatchLoading = false;
  isLoadPaused = false;
  currentBatchAction = null;
  batchQueue = [];
  BatchSSE.close(batchEventSource);
  batchEventSource = null;
  currentBatchTaskId = null;
  setOnlineStatus('已终止', 'text-xs text-[var(--accents-4)] mt-1');
  updateLoadButton();
  refreshBatchUI();
  if (!options.silent) showToast('已终止剩余加载请求', 'info');
}

function stopBatchDelete(options = {}) {
  if (!isBatchDeleting) return;
  isBatchDeleting = false;
  isDeletePaused = false;
  currentBatchAction = null;
  batchQueue = [];
  BatchSSE.close(batchEventSource);
  batchEventSource = null;
  currentBatchTaskId = null;
  updateDeleteButton();
  refreshBatchUI();
  if (!options.silent) showToast('已终止剩余清理请求', 'info');
}

function togglePause() {
  if (isBatchLoading || isBatchDeleting) {
    showToast('当前批量任务不支持暂停', 'info');
  }
}

function stopActiveBatch() {
  if (isBatchLoading) {
    BatchSSE.cancel(currentBatchTaskId, apiKey);
    stopBatchLoad();
  } else if (isBatchDeleting) {
    BatchSSE.cancel(currentBatchTaskId, apiKey);
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

async function startBatchLoad(tokens) {
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
  if (accountMap.size > 0) {
    renderAccountTable({ online_accounts: Array.from(accountMap.values()), online_details: [], online: {} });
  }
  refreshBatchUI();

  try {
    const res = await fetch('/api/v1/admin/cache/online/load/async', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        ...buildAuthHeaders(apiKey)
      },
      body: JSON.stringify({ tokens })
    });
    const data = await res.json();
    if (!res.ok || data.status !== 'success') {
      throw new Error(data.detail || '请求失败');
    }

    currentBatchTaskId = data.task_id;
    BatchSSE.close(batchEventSource);
    batchEventSource = BatchSSE.open(currentBatchTaskId, apiKey, {
      onMessage: (msg) => {
        if (msg.type === 'snapshot' || msg.type === 'progress') {
          if (typeof msg.total === 'number') batchTotal = msg.total;
          if (typeof msg.processed === 'number') batchProcessed = msg.processed;
          updateBatchProgress();
        } else if (msg.type === 'done') {
          if (typeof msg.total === 'number') batchTotal = msg.total;
          batchProcessed = batchTotal;
          updateBatchProgress();
          const result = msg.result;
          if (result) {
            applyStatsData(result, true);
            const details = Array.isArray(result.online_details) ? result.online_details : [];
            loadFailed.clear();
            details.forEach(detail => {
              if (detail.status !== 'ok') loadFailed.set(detail.token, detail.status);
            });
          }
          finishBatchLoad();
          if (msg.warning) {
            showToast(`加载完成\n⚠️ ${msg.warning}`, 'warning');
          }
          currentBatchTaskId = null;
          BatchSSE.close(batchEventSource);
          batchEventSource = null;
        } else if (msg.type === 'cancelled') {
          stopBatchLoad({ silent: true });
          showToast('已终止加载', 'info');
          currentBatchTaskId = null;
          BatchSSE.close(batchEventSource);
          batchEventSource = null;
        } else if (msg.type === 'error') {
          stopBatchLoad({ silent: true });
          showToast('加载失败: ' + (msg.error || '未知错误'), 'error');
          currentBatchTaskId = null;
          BatchSSE.close(batchEventSource);
          batchEventSource = null;
        }
      },
      onError: () => {
        stopBatchLoad({ silent: true });
        showToast('连接中断', 'error');
        currentBatchTaskId = null;
        BatchSSE.close(batchEventSource);
        batchEventSource = null;
      }
    });
  } catch (e) {
    stopBatchLoad({ silent: true });
    showToast(e.message || '请求失败', 'error');
  }
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
  refreshBatchUI();
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

async function startBatchDelete(tokens) {
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
  refreshBatchUI();
  try {
    const res = await fetch('/api/v1/admin/cache/online/clear/async', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        ...buildAuthHeaders(apiKey)
      },
      body: JSON.stringify({ tokens })
    });
    const data = await res.json();
    if (!res.ok || data.status !== 'success') {
      throw new Error(data.detail || '请求失败');
    }

    currentBatchTaskId = data.task_id;
    BatchSSE.close(batchEventSource);
    batchEventSource = BatchSSE.open(currentBatchTaskId, apiKey, {
      onMessage: (msg) => {
        if (msg.type === 'snapshot' || msg.type === 'progress') {
          if (typeof msg.total === 'number') deleteTotal = msg.total;
          if (typeof msg.processed === 'number') deleteProcessed = msg.processed;
          updateBatchProgress();
        } else if (msg.type === 'done') {
          if (typeof msg.total === 'number') deleteTotal = msg.total;
          deleteProcessed = deleteTotal;
          updateBatchProgress();
          const result = msg.result;
          deleteFailed.clear();
          if (result && result.results) {
            Object.entries(result.results).forEach(([token, res]) => {
              if (res.status !== 'success') {
                deleteFailed.set(token, res.error || '清理失败');
              }
            });
          }
          finishBatchDelete();
          if (msg.warning) {
            showToast(`清理完成\n⚠️ ${msg.warning}`, 'warning');
          }
          currentBatchTaskId = null;
          BatchSSE.close(batchEventSource);
          batchEventSource = null;
        } else if (msg.type === 'cancelled') {
          stopBatchDelete({ silent: true });
          showToast('已终止清理', 'info');
          currentBatchTaskId = null;
          BatchSSE.close(batchEventSource);
          batchEventSource = null;
        } else if (msg.type === 'error') {
          stopBatchDelete({ silent: true });
          showToast('清理失败: ' + (msg.error || '未知错误'), 'error');
          currentBatchTaskId = null;
          BatchSSE.close(batchEventSource);
          batchEventSource = null;
        }
      },
      onError: () => {
        stopBatchDelete({ silent: true });
        showToast('连接中断', 'error');
        currentBatchTaskId = null;
        BatchSSE.close(batchEventSource);
        batchEventSource = null;
      }
    });
  } catch (e) {
    stopBatchDelete({ silent: true });
    showToast(e.message || '请求失败', 'error');
  }
}

function finishBatchDelete() {
  isBatchDeleting = false;
  isDeletePaused = false;
  currentBatchAction = null;
  updateDeleteButton();
  refreshBatchUI();
  showToast('批量清理完成', 'success');
  loadStats();
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
