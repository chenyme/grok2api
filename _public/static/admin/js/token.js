let apiKey = '';
let consumedModeEnabled = false;
let allTokens = {};
let flatTokens = [];
let isBatchProcessing = false;
let isBatchPaused = false;
let batchQueue = [];
let batchTotal = 0;
let batchProcessed = 0;
let currentBatchAction = null;
let currentFilter = 'all';
let currentBatchTaskId = null;
let batchEventSource = null;
let currentPage = 1;
let pageSize = 50;
let currentTotalCount = 0;
let currentTotalPages = 1;
const selectedTokenKeys = new Set();

const byId = (id) => document.getElementById(id);
const qsa = (selector) => document.querySelectorAll(selector);
const DEFAULT_QUOTA_BASIC = 80;
const DEFAULT_QUOTA_SUPER = 140;

function getDefaultQuotaForPool(pool) {
  return pool === 'ssoSuper' ? DEFAULT_QUOTA_SUPER : DEFAULT_QUOTA_BASIC;
}

function getTokenRowKey(poolOrItem, tokenValue = null) {
  if (poolOrItem && typeof poolOrItem === 'object') {
    return JSON.stringify([poolOrItem.pool || '', poolOrItem.token || '']);
  }
  return JSON.stringify([poolOrItem || '', tokenValue || '']);
}

function parseTokenRowKey(key) {
  try {
    const parsed = JSON.parse(key);
    if (!Array.isArray(parsed) || parsed.length !== 2) return null;
    const [pool, token] = parsed;
    if (!token) return null;
    return {
      pool: String(pool || ''),
      token: String(token)
    };
  } catch {
    return null;
  }
}

function sameTokenRef(left, right) {
  return !!left
    && !!right
    && String(left.token || '') === String(right.token || '')
    && String(left.pool || '') === String(right.pool || '');
}

function setText(id, text) {
  const el = byId(id);
  if (el) el.innerText = text;
}

function openModal(id) {
  const modal = byId(id);
  if (!modal) return null;
  modal.classList.remove('hidden');
  requestAnimationFrame(() => {
    modal.classList.add('is-open');
  });
  return modal;
}

function closeModal(id, onClose) {
  const modal = byId(id);
  if (!modal) return;
  modal.classList.remove('is-open');
  setTimeout(() => {
    modal.classList.add('hidden');
    if (onClose) onClose();
  }, 200);
}

function downloadTextFile(content, filename) {
  const blob = new Blob([content], { type: 'text/plain' });
  const url = window.URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  window.URL.revokeObjectURL(url);
  document.body.removeChild(a);
}

async function readJsonResponse(res) {
  const text = await res.text();
  if (!text) return null;
  try {
    return JSON.parse(text);
  } catch (err) {
    throw new Error(t('token.notValidJson', { status: res.status }));
  }
}

function normalizeTokenStatus(status) {
  const raw = String(status || 'active').trim();
  if (!raw) return 'active';
  const normalized = raw.startsWith('TokenStatus.')
    ? raw.slice('TokenStatus.'.length)
    : raw;
  return normalized.toLowerCase();
}

function normalizeTokenItem(pool, tokenData, selected = false) {
  const normalized = typeof tokenData === 'string'
    ? { token: tokenData, status: 'active', quota: 0, note: '', use_count: 0, tags: [] }
    : {
      token: tokenData.token,
      status: normalizeTokenStatus(tokenData.status),
      quota: tokenData.quota || 0,
      consumed: tokenData.consumed || 0,
      note: tokenData.note || '',
      fail_count: tokenData.fail_count || 0,
      use_count: tokenData.use_count || 0,
      tags: tokenData.tags || [],
      created_at: tokenData.created_at,
      last_used_at: tokenData.last_used_at,
      last_fail_at: tokenData.last_fail_at,
      last_fail_reason: tokenData.last_fail_reason,
      last_sync_at: tokenData.last_sync_at,
      last_asset_clear_at: tokenData.last_asset_clear_at
    };
  const rowKey = getTokenRowKey(pool, normalized.token);
  return { ...normalized, pool, _rowKey: rowKey, _selected: selected };
}

function normalizeTokensByPool(data, selectedIds = selectedTokenKeys) {
  const items = [];
  Object.keys(data || {}).forEach((pool) => {
    const tokens = data[pool];
    if (!Array.isArray(tokens)) return;
    tokens.forEach((tokenData) => {
      const token = typeof tokenData === 'string' ? tokenData : (tokenData.token || '');
      const rowKey = getTokenRowKey(pool, token);
      items.push(normalizeTokenItem(pool, tokenData, selectedIds.has(rowKey)));
    });
  });
  return items;
}

function buildTokensPayload(items) {
  const payload = {};
  items.forEach((item) => {
    if (!payload[item.pool]) payload[item.pool] = [];
    const tokenPayload = {
      token: item.token,
      status: item.status,
      quota: item.quota,
      consumed: item.consumed || 0,
      note: item.note,
      fail_count: item.fail_count,
      use_count: item.use_count || 0,
      tags: Array.isArray(item.tags) ? item.tags : []
    };
    if (typeof item.created_at === 'number') tokenPayload.created_at = item.created_at;
    if (typeof item.last_used_at === 'number') tokenPayload.last_used_at = item.last_used_at;
    if (typeof item.last_fail_at === 'number') tokenPayload.last_fail_at = item.last_fail_at;
    if (typeof item.last_sync_at === 'number') tokenPayload.last_sync_at = item.last_sync_at;
    if (typeof item.last_asset_clear_at === 'number') tokenPayload.last_asset_clear_at = item.last_asset_clear_at;
    if (typeof item.last_fail_reason === 'string' && item.last_fail_reason) tokenPayload.last_fail_reason = item.last_fail_reason;
    payload[item.pool].push(tokenPayload);
  });
  return payload;
}

async function fetchAllTokenItems() {
  const res = await fetch('/v1/admin/tokens', {
    headers: buildAuthHeaders(apiKey)
  });
  if (res.status === 401) {
    logout();
    throw new Error('Unauthorized');
  }
  if (!res.ok) {
    throw new Error(`HTTP ${res.status}`);
  }
  const data = await res.json();
  consumedModeEnabled = data.consumed_mode_enabled || false;
  updateQuotaHeader();
  return normalizeTokensByPool(data.tokens || {});
}

function getSelectedTokens() {
  return getSelectedTokenRefs().map((item) => item.token);
}

function getSelectedTokenRefs() {
  return Array.from(selectedTokenKeys)
    .map((key) => parseTokenRowKey(key))
    .filter((item) => item && item.token);
}

function countSelected(tokens) {
  let count = 0;
  for (const token of tokens || []) {
    if (token && token._selected) count++;
  }
  return count;
}

function setSelectedForTokens(tokens, selected) {
  (tokens || []).forEach((token) => {
    token._selected = selected;
    if (!token?._rowKey) return;
    if (selected) {
      selectedTokenKeys.add(token._rowKey);
    } else {
      selectedTokenKeys.delete(token._rowKey);
    }
  });
}

function syncVisibleSelectionUI(selected) {
  qsa('#token-table-body input[type="checkbox"]').forEach(input => {
    input.checked = selected;
  });
  qsa('#token-table-body tr').forEach(row => {
    row.classList.toggle('row-selected', selected);
  });
}

function getVisibleTokens() {
  return flatTokens;
}

async function init() {
  apiKey = await ensureAdminKey();
  if (apiKey === null) return;
  setupEditPoolDefaults();
  setupConfirmDialog();
  setupSelectAllMenu();
  refreshPageSizeOptionsI18n();
  await loadData();
}

function getPagedTokensUrl() {
  const params = new URLSearchParams();
  params.set('page', String(currentPage));
  params.set('page_size', String(pageSize));
  params.set('filter', currentFilter);
  return `/v1/admin/tokens?${params.toString()}`;
}

async function loadData() {
  try {
    const res = await fetch(getPagedTokensUrl(), {
      headers: buildAuthHeaders(apiKey)
    });
    if (res.ok) {
      const data = await res.json();
      consumedModeEnabled = data.consumed_mode_enabled || false;
      currentPage = Number(data.page || currentPage) || 1;
      pageSize = Number(data.page_size || pageSize) || 50;
      currentTotalCount = Number(data.total || 0);
      currentTotalPages = Math.max(1, Number(data.total_pages || 1));
      flatTokens = (data.items || []).map((item) => normalizeTokenItem(
        item.pool,
        item,
        selectedTokenKeys.has(getTokenRowKey(item.pool, item.token))
      ));
      updateQuotaHeader();
      updateStats(data.summary || {});
      updateTabCounts(data.counts || {});
      renderTable();
    } else if (res.status === 401) {
      logout();
    } else {
      throw new Error(`HTTP ${res.status}`);
    }
  } catch (e) {
    showToast(t('common.loadError', { msg: e.message }), 'error');
  }
}

function updateQuotaHeader() {
  const thQuota = document.getElementById('th-quota');
  if (thQuota) {
    if (consumedModeEnabled) {
      thQuota.textContent = t('token.tableQuotaConsumed');
      thQuota.dataset.i18n = 'token.tableQuotaConsumed';
    } else {
      thQuota.textContent = t('token.tableQuota');
      thQuota.dataset.i18n = 'token.tableQuota';
    }
  }
}

function updateStats(summary) {
  const safeSummary = summary || {};
  const totalTokens = Number(safeSummary.total || 0);
  const activeTokens = Number(safeSummary.active || 0);
  const coolingTokens = Number(safeSummary.cooling || 0);
  const invalidTokens = Number(safeSummary.invalid || 0);
  const chatQuota = Number(safeSummary.chat_quota || 0);
  const imageQuota = Number(safeSummary.image_quota || 0);
  const totalConsumed = Number(safeSummary.total_consumed || 0);
  const totalCalls = Number(safeSummary.total_calls || 0);

  setText('stat-total', totalTokens.toLocaleString());
  setText('stat-active', activeTokens.toLocaleString());
  setText('stat-cooling', coolingTokens.toLocaleString());
  setText('stat-invalid', invalidTokens.toLocaleString());

  if (consumedModeEnabled) {
    setText('stat-chat-quota', totalConsumed.toLocaleString());
    setText('stat-image-quota', Math.floor(totalConsumed / 2).toLocaleString());
    const chatLabel = document.querySelector('[data-i18n="token.statChatQuota"]');
    const imageLabel = document.querySelector('[data-i18n="token.statImageQuota"]');
    if (chatLabel) chatLabel.textContent = t('token.statChatConsumed');
    if (imageLabel) imageLabel.textContent = t('token.statImageConsumed');
  } else {
    setText('stat-chat-quota', chatQuota.toLocaleString());
    setText('stat-image-quota', imageQuota.toLocaleString());
  }

  setText('stat-total-calls', totalCalls.toLocaleString());
}

function renderTable() {
  const tbody = byId('token-table-body');
  const loading = byId('loading');
  const emptyState = byId('empty-state');

  if (loading) loading.classList.add('hidden');

  const visibleTokens = flatTokens;
  updatePaginationControls(currentTotalCount, currentTotalPages);

  if (visibleTokens.length === 0) {
    tbody.replaceChildren();
    if (emptyState) {
      emptyState.textContent = currentFilter === 'all'
        ? t('token.emptyState')
        : t('token.emptyFilterState');
    }
    emptyState.classList.remove('hidden');
    updateSelectionState();
    return;
  }
  emptyState.classList.add('hidden');

  const fragment = document.createDocumentFragment();
  visibleTokens.forEach((item, originalIndex) => {
    const tr = document.createElement('tr');
    tr.dataset.index = originalIndex;
    if (item._selected) tr.classList.add('row-selected');

    const tdCheck = document.createElement('td');
    tdCheck.className = 'text-center';
    tdCheck.innerHTML = `<input type="checkbox" class="checkbox" ${item._selected ? 'checked' : ''} onchange="toggleSelect(${originalIndex})">`;

    const tdToken = document.createElement('td');
    tdToken.className = 'text-left';
    const tokenShort = item.token.length > 24
      ? item.token.substring(0, 8) + '...' + item.token.substring(item.token.length - 16)
      : item.token;
    tdToken.innerHTML = `
                <div class="flex items-center gap-2">
                    <span class="font-mono text-xs text-gray-500" title="${item.token}">${tokenShort}</span>
                    <button class="text-gray-400 hover:text-black transition-colors" onclick="copyToClipboard('${item.token}', this)">
                        <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="13" height="13" rx="2" ry="2"></rect><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"></path></svg>
                    </button>
                </div>
             `;

    const tdType = document.createElement('td');
    tdType.className = 'text-center';
    tdType.innerHTML = `<span class="badge badge-gray">${escapeHtml(item.pool)}</span>`;

    const tdStatus = document.createElement('td');
    let statusClass = 'badge-gray';
    if (item.status === 'active') statusClass = 'badge-green';
    else if (item.status === 'cooling') statusClass = 'badge-orange';
    else if (item.status === 'expired') statusClass = 'badge-red';
    else statusClass = 'badge-gray';
    tdStatus.className = 'text-center';
    let statusHtml = `<span class="badge ${statusClass}">${item.status}</span>`;
    if (item.tags && item.tags.includes('nsfw')) {
      statusHtml += ` <span class="badge badge-purple">nsfw</span>`;
    }
    tdStatus.innerHTML = statusHtml;

    const tdQuota = document.createElement('td');
    tdQuota.className = 'text-center font-mono text-xs';
    if (consumedModeEnabled) {
      tdQuota.innerText = item.consumed;
      tdQuota.title = t('token.tableQuotaConsumed');
    } else {
      tdQuota.innerText = item.quota;
      tdQuota.title = t('token.tableQuota');
    }

    const tdNote = document.createElement('td');
    tdNote.className = 'text-left text-gray-500 text-xs truncate max-w-[150px]';
    tdNote.innerText = item.note || '-';

    const tdActions = document.createElement('td');
    tdActions.className = 'text-center';
    const isDisabled = item.status === 'disabled';
    const toggleTitle = isDisabled ? t('token.enableToken') : t('token.disableToken');
    const toggleIcon = isDisabled
      ? '<polyline points="20 6 9 17 4 12"></polyline>'
      : '<line x1="18" y1="6" x2="6" y2="18"></line><line x1="6" y1="6" x2="18" y2="18"></line>';
    const toggleClass = isDisabled
      ? 'p-1 text-gray-400 hover:text-green-600 rounded'
      : 'p-1 text-gray-400 hover:text-orange-600 rounded';
    tdActions.innerHTML = `
                <div class="flex items-center justify-center gap-2">
                     <button onclick="refreshStatus(${originalIndex})" class="p-1 text-gray-400 hover:text-black rounded" title="${t('token.refreshStatus')}">
                        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="23 4 23 10 17 10"></polyline><polyline points="1 20 1 14 7 14"></polyline><path d="M3.51 9a9 9 0 0 1 14.85-3.36L23 10M1 14l4.64 4.36A9 9 0 0 0 20.49 15"></path></svg>
                     </button>
                     <button onclick="toggleTokenEnabled(${originalIndex})" class="${toggleClass}" title="${toggleTitle}">
                        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">${toggleIcon}</svg>
                     </button>
                     <button onclick="openEditModal(${originalIndex})" class="p-1 text-gray-400 hover:text-black rounded" title="${t('common.edit')}">
                        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M11 4H4a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-7"></path><path d="M18.5 2.5a2.121 2.121 0 0 1 3 3L12 15l-4 1 1-4 9.5-9.5z"></path></svg>
                     </button>
                     <button onclick="deleteToken(${originalIndex})" class="p-1 text-gray-400 hover:text-red-600 rounded" title="${t('common.delete')}">
                        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="3 6 5 6 21 6"></polyline><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"></path></svg>
                     </button>
                </div>
             `;

    tr.appendChild(tdCheck);
    tr.appendChild(tdToken);
    tr.appendChild(tdType);
    tr.appendChild(tdStatus);
    tr.appendChild(tdQuota);
    tr.appendChild(tdNote);
    tr.appendChild(tdActions);

    fragment.appendChild(tr);
  });

  tbody.replaceChildren(fragment);
  updateSelectionState();
}

// Selection Logic
function toggleSelectAll() {
  const checkbox = byId('select-all');
  const checked = !!(checkbox && checkbox.checked);
  // 只选择当前页可见的 Token
  setSelectedForTokens(getVisibleTokens(), checked);
  syncVisibleSelectionUI(checked);
  updateSelectionState();
}

function closeSelectAllMenu() {
  const popover = byId('select-all-popover');
  if (popover) popover.classList.add('hidden');
}

function openSelectAllMenu() {
  const popover = byId('select-all-popover');
  if (popover) popover.classList.remove('hidden');
}

function isSelectAllMenuOpen() {
  const popover = byId('select-all-popover');
  return !!(popover && !popover.classList.contains('hidden'));
}

function setupSelectAllMenu() {
  document.addEventListener('click', (event) => {
    const wrap = byId('select-all-wrap');
    if (!wrap) return;
    if (wrap.contains(event.target)) return;
    closeSelectAllMenu();
  });
  document.addEventListener('keydown', (event) => {
    if (event.key === 'Escape') {
      closeSelectAllMenu();
    }
  });
}

function handleSelectAllPrimary(event) {
  if (event) event.stopPropagation();
  const selected = selectedTokenKeys.size;
  if (selected > 0) {
    clearAllSelection();
    return;
  }
  if (isSelectAllMenuOpen()) {
    closeSelectAllMenu();
  } else {
    openSelectAllMenu();
  }
}

function selectVisibleAllFromMenu() {
  selectVisibleAll();
  closeSelectAllMenu();
}

function selectAllFilteredFromMenu() {
  selectAllFiltered();
}

async function selectAllFiltered() {
  try {
    const params = new URLSearchParams();
    params.set('filter', currentFilter);
    params.set('keys_only', 'true');
    const res = await fetch(`/v1/admin/tokens?${params.toString()}`, {
      headers: buildAuthHeaders(apiKey)
    });
    if (res.status === 401) {
      logout();
      return;
    }
    if (!res.ok) {
      throw new Error(`HTTP ${res.status}`);
    }
    const data = await res.json();
    const items = Array.isArray(data.items) ? data.items : [];
    if (items.length === 0) return;
    items.forEach((item) => {
      if (item?.token) selectedTokenKeys.add(getTokenRowKey(item.pool, item.token));
    });
    flatTokens.forEach((item) => {
      item._selected = selectedTokenKeys.has(item._rowKey);
    });
    syncVisibleSelectionUI(flatTokens.length > 0 && flatTokens.every((item) => item._selected));
    updateSelectionState();
    closeSelectAllMenu();
  } catch (e) {
    showToast(t('common.loadError', { msg: e.message }), 'error');
  }
}

function selectVisibleAll() {
  const visible = getVisibleTokens();
  if (visible.length === 0) return;
  setSelectedForTokens(visible, true);
  syncVisibleSelectionUI(true);
  updateSelectionState();
  closeSelectAllMenu();
}

function clearAllSelection() {
  if (selectedTokenKeys.size === 0 && flatTokens.length === 0) return;
  selectedTokenKeys.clear();
  setSelectedForTokens(flatTokens, false);
  syncVisibleSelectionUI(false);
  updateSelectionState();
  closeSelectAllMenu();
}

function toggleSelect(index) {
  const item = flatTokens[index];
  if (!item) return;
  item._selected = !item._selected;
  if (item._selected) {
    selectedTokenKeys.add(item._rowKey);
  } else {
    selectedTokenKeys.delete(item._rowKey);
  }
  const row = document.querySelector(`#token-table-body tr[data-index="${index}"]`);
  if (row) row.classList.toggle('row-selected', item._selected);
  updateSelectionState();
}

function updateSelectionState() {
  const selectedCount = selectedTokenKeys.size;
  const visible = getVisibleTokens();
  const visibleSelected = countSelected(visible);
  const selectAll = byId('select-all');
  if (selectAll) {
    const hasVisible = visible.length > 0;
    selectAll.disabled = !hasVisible;
    selectAll.checked = hasVisible && visibleSelected === visible.length;
    selectAll.indeterminate = visibleSelected > 0 && visibleSelected < visible.length;
  }
  const selectedCountEl = byId('selected-count');
  if (selectedCountEl) selectedCountEl.innerText = selectedCount;
  const selectAllLabel = byId('select-all-label');
  const selectAllTrigger = byId('select-all-trigger');
  const selectAllCaret = byId('select-all-caret');
  if (selectAllLabel) {
    selectAllLabel.textContent = selectedCount > 0
      ? t('token.clearSelection')
      : t('common.selectAll');
  }
  if (selectAllTrigger) {
    selectAllTrigger.classList.toggle('is-active', selectedCount > 0);
  }
  if (selectAllCaret) {
    selectAllCaret.style.display = selectedCount > 0 ? 'none' : 'inline';
  }
  if (selectedCount > 0) {
    closeSelectAllMenu();
  }
  setActionButtonsState(selectedCount);
}

// Actions
function addToken() {
  openEditModal(-1);
}

// Batch export (Selected only)
function batchExport() {
  const selected = getSelectedTokens();
  if (selected.length === 0) return showToast(t('common.noTokenSelected'), 'error');
  const content = selected.join('\n') + '\n';
  downloadTextFile(content, `tokens_export_selected_${new Date().toISOString().slice(0, 10)}.txt`);
}


// Modal Logic
let currentEditIndex = -1;
function openEditModal(index) {
  const modal = byId('edit-modal');
  if (!modal) return;

  currentEditIndex = index;

  if (index >= 0) {
    // Edit existing
    const item = flatTokens[index];
    byId('edit-token-display').value = item.token;
    byId('edit-original-token').value = item.token;
    byId('edit-original-pool').value = item.pool;
    byId('edit-pool').value = item.pool;
    byId('edit-note').value = item.note;

    // 根据配置决定是否禁用 quota 编辑
    const quotaInput = byId('edit-quota');
    const quotaField = quotaInput?.closest('div');
    const quotaLabel = quotaField?.querySelector('label');
    if (consumedModeEnabled) {
      quotaInput.value = item.consumed || 0;
      quotaInput.disabled = true;
      quotaInput.classList.add('bg-gray-100', 'text-gray-400');
      if (quotaLabel) quotaLabel.textContent = t('token.tableQuotaConsumed');
    } else {
      quotaInput.value = item.quota;
      quotaInput.disabled = false;
      quotaInput.classList.remove('bg-gray-100', 'text-gray-400');
      if (quotaLabel) quotaLabel.textContent = t('token.editQuota');
    }

    document.querySelector('#edit-modal h3').innerText = t('token.editTitle');
  } else {
    // New Token
    const tokenInput = byId('edit-token-display');
    tokenInput.value = '';
    tokenInput.disabled = false;
    tokenInput.placeholder = 'sk-...';
    tokenInput.classList.remove('bg-gray-50', 'text-gray-500');

    byId('edit-original-token').value = '';
    byId('edit-original-pool').value = '';
    byId('edit-pool').value = 'ssoBasic';
    byId('edit-quota').value = getDefaultQuotaForPool('ssoBasic');
    byId('edit-note').value = '';
    document.querySelector('#edit-modal h3').innerText = t('token.addTitle');

    // 新建 Token 时启用 quota 编辑
    const newQuotaInput = byId('edit-quota');
    const newQuotaField = newQuotaInput?.closest('div');
    const newQuotaLabel = newQuotaField?.querySelector('label');
    newQuotaInput.disabled = false;
    newQuotaInput.classList.remove('bg-gray-100', 'text-gray-400');
    if (newQuotaLabel) newQuotaLabel.textContent = t('token.editQuota');
  }

  openModal('edit-modal');
}

function setupEditPoolDefaults() {
  const poolSelect = byId('edit-pool');
  const quotaInput = byId('edit-quota');
  if (!poolSelect || !quotaInput) return;
  poolSelect.addEventListener('change', () => {
    if (currentEditIndex >= 0) return;
    quotaInput.value = getDefaultQuotaForPool(poolSelect.value);
  });
}

function closeEditModal() {
  closeModal('edit-modal', () => {
    // reset styles for token input
    const input = byId('edit-token-display');
    if (input) {
      input.disabled = true;
      input.classList.add('bg-gray-50', 'text-gray-500');
    }
  });
}

async function saveEdit() {
  const newPool = byId('edit-pool').value.trim();
  const quotaFieldValue = parseInt(byId('edit-quota').value, 10);
  const newNote = byId('edit-note').value.trim().slice(0, 50);
  const allItems = await fetchAllTokenItems();

  if (currentEditIndex >= 0) {
    const item = flatTokens[currentEditIndex];
    if (!item) return;
    const newQuota = consumedModeEnabled
      ? item.quota
      : (Number.isNaN(quotaFieldValue) ? 0 : quotaFieldValue);
    const target = allItems.find((tokenItem) => sameTokenRef(tokenItem, item));
    if (!target) {
      showToast(t('common.loadError', { msg: 'Token not found' }), 'error');
      return;
    }
    const nextRef = { pool: newPool || 'ssoBasic', token: item.token };
    const hasCollision = allItems.some((tokenItem) => !sameTokenRef(tokenItem, item) && sameTokenRef(tokenItem, nextRef));
    if (hasCollision) {
      showToast(t('token.tokenExists'), 'error');
      return;
    }
    target.pool = nextRef.pool;
    target.quota = newQuota;
    target.note = newNote;
  } else {
    const newQuota = Number.isNaN(quotaFieldValue) ? 0 : quotaFieldValue;
    const token = byId('edit-token-display').value.trim();
    if (!token) return showToast(t('token.tokenEmpty'), 'error');
    const nextRef = { pool: newPool || 'ssoBasic', token };
    if (allItems.some((item) => sameTokenRef(item, nextRef))) {
      return showToast(t('token.tokenExists'), 'error');
    }
    allItems.push({
      token: token,
      pool: nextRef.pool,
      quota: newQuota,
      consumed: 0,
      note: newNote,
      status: 'active',
      use_count: 0,
      tags: [],
      fail_count: 0,
      _rowKey: getTokenRowKey(nextRef),
      _selected: false
    });
  }

  selectedTokenKeys.clear();
  await syncToServer(allItems);
  closeEditModal();
  await loadData();
}

async function deleteToken(index) {
  const ok = await confirmAction(t('token.confirmDelete'), { okText: t('common.delete') });
  if (!ok) return;
  const item = flatTokens[index];
  if (!item) return;
  const allItems = await fetchAllTokenItems();
  const nextItems = allItems.filter((tokenItem) => !sameTokenRef(tokenItem, item));
  selectedTokenKeys.clear();
  await syncToServer(nextItems);
  await loadData();
}

async function toggleTokenEnabled(index) {
  const item = flatTokens[index];
  if (!item) return;
  const toDisabled = item.status !== 'disabled';
  const targetStatus = toDisabled ? 'disabled' : 'active';
  const confirmKey = toDisabled ? 'token.confirmDisable' : 'token.confirmEnable';
  const okText = toDisabled ? t('token.disableToken') : t('token.enableToken');
  const tokenLabel = item.token.length > 24
    ? `${item.token.substring(0, 8)}...${item.token.substring(item.token.length - 16)}`
    : item.token;
  const ok = await confirmAction(t(confirmKey, { token: tokenLabel }), { okText });
  if (!ok) return;
  const allItems = await fetchAllTokenItems();
  const target = allItems.find((tokenItem) => sameTokenRef(tokenItem, item));
  if (!target) {
    showToast(t('common.loadError', { msg: 'Token not found' }), 'error');
    return;
  }
  target.status = targetStatus;
  selectedTokenKeys.clear();
  await syncToServer(allItems);
  await loadData();
  showToast(toDisabled ? t('token.disableDone') : t('token.enableDone'), 'success');
}

function batchDelete() {
  startBatchDelete();
}

function _getBatchStatusTargets(targetStatus) {
  const selected = getSelectedTokenRefs();
  return { selected, targetStatus };
}

async function batchSetStatus(targetStatus) {
  if (isBatchProcessing) {
    showToast(t('common.taskInProgress'), 'info');
    return;
  }
  const { selected } = _getBatchStatusTargets(targetStatus);
  if (selected.length === 0) {
    showToast(t('common.noTokenSelected'), 'error');
    return;
  }
  const toDisabled = targetStatus === 'disabled';
  const allItems = await fetchAllTokenItems();
  const targets = allItems.filter((item) => selectedTokenKeys.has(item._rowKey) && item.status !== targetStatus);
  if (targets.length === 0) {
    showToast(toDisabled ? t('token.noTokenToDisable') : t('token.noTokenToEnable'), 'info');
    return;
  }
  const confirmKey = toDisabled ? 'token.confirmBatchDisable' : 'token.confirmBatchEnable';
  const okText = toDisabled ? t('token.batchDisable') : t('token.batchEnable');
  const ok = await confirmAction(t(confirmKey, { count: targets.length }), { okText });
  if (!ok) return;
  targets.forEach(item => {
    item.status = targetStatus;
  });
  selectedTokenKeys.clear();
  await syncToServer(allItems);
  await loadData();
  showToast(toDisabled ? t('token.batchDisableDone') : t('token.batchEnableDone'), 'success');
}

async function batchDisableTokens() {
  await batchSetStatus('disabled');
}

async function batchEnableTokens() {
  await batchSetStatus('active');
}

async function syncToServer(items) {
  const newTokens = buildTokensPayload(items || []);
  try {
    const res = await fetch('/v1/admin/tokens', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        ...buildAuthHeaders(apiKey)
      },
      body: JSON.stringify(newTokens)
    });
    if (!res.ok) showToast(t('common.saveFailed'), 'error');
  } catch (e) {
    showToast(t('common.saveError', { msg: e.message }), 'error');
  }
}

// Import Logic
function openImportModal() {
  openModal('import-modal');
}

function closeImportModal() {
  closeModal('import-modal', () => {
    const input = byId('import-text');
    if (input) input.value = '';
  });
}

async function submitImport() {
  const pool = byId('import-pool').value.trim() || 'ssoBasic';
  const text = byId('import-text').value;
  const lines = text.split('\n');
  const defaultQuota = getDefaultQuotaForPool(pool);
  const allItems = await fetchAllTokenItems();
  const existing = new Set(allItems.map((item) => item._rowKey));

  lines.forEach(line => {
    const t = line.trim();
    const rowKey = getTokenRowKey(pool, t);
    if (t && !existing.has(rowKey)) {
      existing.add(rowKey);
      allItems.push({
        token: t,
        pool: pool,
        status: 'active',
        quota: defaultQuota,
        consumed: 0,
        note: '',
        tags: [],
        fail_count: 0,
        use_count: 0,
        _rowKey: rowKey,
        _selected: false
      });
    }
  });

  selectedTokenKeys.clear();
  await syncToServer(allItems);
  closeImportModal();
  await loadData();
}

// Export Logic
async function exportTokens() {
  const allItems = await fetchAllTokenItems();
  if (allItems.length === 0) return showToast(t('token.listEmpty'), 'error');
  const content = allItems.map(t => t.token).join('\n') + '\n';
  downloadTextFile(content, `tokens_export_${new Date().toISOString().slice(0, 10)}.txt`);
}

async function copyToClipboard(text, btn) {
  if (!text) return;
  try {
    await navigator.clipboard.writeText(text);
    const originalHtml = btn.innerHTML;
    btn.innerHTML = `<svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="20 6 9 17 4 12"></polyline></svg>`;
    btn.classList.remove('text-gray-400');
    btn.classList.add('text-green-500');
    setTimeout(() => {
      btn.innerHTML = originalHtml;
      btn.classList.add('text-gray-400');
      btn.classList.remove('text-green-500');
    }, 2000);
  } catch (err) {
    console.error('Copy failed', err);
  }
}

async function refreshStatus(index) {
  try {
    const item = flatTokens[index];
    if (!item) return;
    const btn = event.currentTarget; // Get button element if triggered by click
    if (btn) {
      btn.innerHTML = `<svg class="animate-spin" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M21 12a9 9 0 1 1-6.219-8.56"></path></svg>`;
    }

    const res = await fetch('/v1/admin/tokens/refresh', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        ...buildAuthHeaders(apiKey)
      },
      body: JSON.stringify({ token: item.token, pool: item.pool })
    });

    const data = await res.json();

    if (res.ok && data.status === 'success') {
      const resultItem = Array.isArray(data.items) ? data.items[0] : null;
      const isSuccess = !!(resultItem && resultItem.ok);
      loadData();

      if (isSuccess) {
        showToast(t('token.refreshSuccess'), 'success');
      } else {
        showToast(t('token.refreshFailed'), 'error');
      }
    } else {
      showToast(t('token.refreshFailed'), 'error');
    }
  } catch (e) {
    console.error(e);
    showToast(t('token.requestError'), 'error');
  }
}


async function startBatchRefresh() {
  if (isBatchProcessing) {
    showToast(t('common.taskInProgress'), 'info');
    return;
  }

  const selected = getSelectedTokenRefs();
  if (selected.length === 0) return showToast(t('common.noTokenSelected'), 'error');

  // Init state
  isBatchProcessing = true;
  isBatchPaused = false;
  currentBatchAction = 'refresh';
  batchQueue = selected.slice();
  batchTotal = batchQueue.length;
  batchProcessed = 0;

  updateBatchProgress();
  setActionButtonsState();

  try {
    const res = await fetch('/v1/admin/tokens/refresh/async', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        ...buildAuthHeaders(apiKey)
      },
      body: JSON.stringify({ tokens: batchQueue })
    });
    const data = await res.json();
    if (!res.ok || data.status !== 'success') {
      throw new Error(data.detail || t('common.requestFailed'));
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
          finishBatchProcess(false, { silent: true });
          if (msg.warning) {
            showToast(t('token.refreshDone') + '\n⚠️ ' + msg.warning, 'warning');
          } else {
            showToast(t('token.refreshDone'), 'success');
          }
          currentBatchTaskId = null;
          BatchSSE.close(batchEventSource);
          batchEventSource = null;
        } else if (msg.type === 'cancelled') {
          finishBatchProcess(true, { silent: true });
          showToast(t('token.stopRefresh'), 'info');
          currentBatchTaskId = null;
          BatchSSE.close(batchEventSource);
          batchEventSource = null;
        } else if (msg.type === 'error') {
          finishBatchProcess(true, { silent: true });
          showToast(t('token.refreshError', { msg: msg.error || t('common.unknownError') }), 'error');
          currentBatchTaskId = null;
          BatchSSE.close(batchEventSource);
          batchEventSource = null;
        }
      },
      onError: () => {
        finishBatchProcess(true, { silent: true });
        showToast(t('common.connectionInterrupted'), 'error');
        currentBatchTaskId = null;
        BatchSSE.close(batchEventSource);
        batchEventSource = null;
      }
    });
  } catch (e) {
    finishBatchProcess(true, { silent: true });
    showToast(e.message || t('common.requestFailed'), 'error');
    currentBatchTaskId = null;
  }
}

function toggleBatchPause() {
  if (!isBatchProcessing) return;
  showToast(t('common.taskNoPause'), 'info');
}

function stopBatchRefresh() {
  if (!isBatchProcessing) return;
  if (currentBatchTaskId) {
    BatchSSE.cancel(currentBatchTaskId, apiKey);
    BatchSSE.close(batchEventSource);
    batchEventSource = null;
    currentBatchTaskId = null;
  }
  finishBatchProcess(true);
}

function finishBatchProcess(aborted = false, options = {}) {
  const action = currentBatchAction;
  isBatchProcessing = false;
  isBatchPaused = false;
  batchQueue = [];
  currentBatchAction = null;
  selectedTokenKeys.clear();
  flatTokens.forEach((token) => {
    token._selected = false;
  });

  updateBatchProgress();
  setActionButtonsState();
  updateSelectionState();
  loadData(); // Final data refresh

  if (options.silent) return;
  if (aborted) {
    if (action === 'delete') {
      showToast(t('token.stopDelete'), 'info');
    } else if (action === 'nsfw') {
      showToast(t('token.stopNsfw'), 'info');
    } else {
      showToast(t('token.stopRefresh'), 'info');
    }
  } else {
    if (action === 'delete') {
      showToast(t('token.deleteDone'), 'success');
    } else if (action === 'nsfw') {
      showToast(t('token.nsfwDone'), 'success');
    } else {
      showToast(t('token.refreshDone'), 'success');
    }
  }
}

async function batchUpdate() {
  startBatchRefresh();
}

function updateBatchProgress() {
  const container = byId('batch-progress');
  const text = byId('batch-progress-text');
  const pauseBtn = byId('btn-pause-action');
  const stopBtn = byId('btn-stop-action');
  if (!container || !text) return;
  if (!isBatchProcessing) {
    container.classList.add('hidden');
    if (pauseBtn) pauseBtn.classList.add('hidden');
    if (stopBtn) stopBtn.classList.add('hidden');
    return;
  }
  const pct = batchTotal ? Math.floor((batchProcessed / batchTotal) * 100) : 0;
  text.textContent = `${pct}%`;
  container.classList.remove('hidden');
  if (pauseBtn) {
    pauseBtn.classList.add('hidden');
  }
  if (stopBtn) stopBtn.classList.remove('hidden');
}

function setActionButtonsState(selectedCount = null) {
  let count = selectedCount;
  if (count === null) {
    count = selectedTokenKeys.size;
  }
  const disabled = isBatchProcessing;
  const exportBtn = byId('btn-batch-export');
  const updateBtn = byId('btn-batch-update');
  const disableBtn = byId('btn-batch-disable');
  const enableBtn = byId('btn-batch-enable');
  const nsfwBtn = byId('btn-batch-nsfw');
  const deleteBtn = byId('btn-batch-delete');
  if (exportBtn) exportBtn.disabled = disabled || count === 0;
  if (updateBtn) updateBtn.disabled = disabled || count === 0;
  if (disableBtn) disableBtn.disabled = disabled || count === 0;
  if (enableBtn) enableBtn.disabled = disabled || count === 0;
  if (nsfwBtn) nsfwBtn.disabled = disabled || count === 0;
  if (deleteBtn) deleteBtn.disabled = disabled || count === 0;
}

async function startBatchDelete() {
  if (isBatchProcessing) {
    showToast(t('common.taskInProgress'), 'info');
    return;
  }
  const selected = getSelectedTokenRefs();
  if (selected.length === 0) return showToast(t('common.noTokenSelected'), 'error');
  const ok = await confirmAction(t('token.confirmBatchDelete', { count: selected.length }), { okText: t('common.delete') });
  if (!ok) return;

  isBatchProcessing = true;
  isBatchPaused = false;
  currentBatchAction = 'delete';
  batchQueue = selected.slice();
  batchTotal = batchQueue.length;
  batchProcessed = 0;

  updateBatchProgress();
  setActionButtonsState();

  try {
    const toRemove = new Set(batchQueue.map((item) => getTokenRowKey(item)));
    const allItems = await fetchAllTokenItems();
    const nextItems = allItems.filter(t => !toRemove.has(t._rowKey));
    toRemove.forEach((tokenKey) => selectedTokenKeys.delete(tokenKey));
    await syncToServer(nextItems);
    batchProcessed = batchTotal;
    updateBatchProgress();
    finishBatchProcess(false, { silent: true });
    showToast(t('token.deleteDone'), 'success');
  } catch (e) {
    finishBatchProcess(true, { silent: true });
    showToast(t('common.deleteFailed'), 'error');
  }
}

let confirmResolver = null;

function setupConfirmDialog() {
  const dialog = byId('confirm-dialog');
  if (!dialog) return;
  const okBtn = byId('confirm-ok');
  const cancelBtn = byId('confirm-cancel');
  dialog.addEventListener('click', (event) => {
    if (event.target === dialog) {
      closeConfirm(false);
    }
  });
  if (okBtn) okBtn.addEventListener('click', () => closeConfirm(true));
  if (cancelBtn) cancelBtn.addEventListener('click', () => closeConfirm(false));
}

function confirmAction(message, options = {}) {
  const dialog = byId('confirm-dialog');
  if (!dialog) {
    return Promise.resolve(false);
  }
  const messageEl = byId('confirm-message');
  const okBtn = byId('confirm-ok');
  const cancelBtn = byId('confirm-cancel');
  if (messageEl) messageEl.textContent = message;
  if (okBtn) okBtn.textContent = options.okText || t('common.ok');
  if (cancelBtn) cancelBtn.textContent = options.cancelText || t('common.cancel');
  return new Promise(resolve => {
    confirmResolver = resolve;
    dialog.classList.remove('hidden');
    requestAnimationFrame(() => {
      dialog.classList.add('is-open');
    });
  });
}

function closeConfirm(ok) {
  const dialog = byId('confirm-dialog');
  if (!dialog) return;
  dialog.classList.remove('is-open');
  setTimeout(() => {
    dialog.classList.add('hidden');
    if (confirmResolver) {
      confirmResolver(ok);
      confirmResolver = null;
    }
  }, 200);
}

function escapeHtml(text) {
  if (!text) return '';
  return text
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#039;");
}

// ========== Tab 筛选功能 ==========

function filterByStatus(status) {
  currentFilter = status;
  currentPage = 1;
  closeSelectAllMenu();

  // 更新 Tab 样式和 ARIA
  document.querySelectorAll('.tab-item').forEach(tab => {
    const isActive = tab.dataset.filter === status;
    tab.classList.toggle('active', isActive);
    tab.setAttribute('aria-selected', isActive ? 'true' : 'false');
  });

  loadData();
}

function getFilteredTokens() {
  return flatTokens;
}

function updateTabCounts(counts) {
  const safeCounts = counts || {
    all: flatTokens.length,
    active: flatTokens.filter(t => t.status === 'active').length,
    cooling: flatTokens.filter(t => t.status === 'cooling').length,
    expired: flatTokens.filter(t => t.status !== 'active' && t.status !== 'cooling').length,
    nsfw: flatTokens.filter(t => t.tags && t.tags.includes('nsfw')).length,
    'no-nsfw': flatTokens.filter(t => !t.tags || !t.tags.includes('nsfw')).length
  };

  Object.entries(safeCounts).forEach(([key, count]) => {
    const el = byId(`tab-count-${key}`);
    if (el) el.textContent = count;
  });
}

function getVisibleTokens() {
  return flatTokens;
}

function refreshPageSizeOptionsI18n() {
  const sizeSelect = byId('page-size');
  if (!sizeSelect) return;
  Array.from(sizeSelect.options).forEach((opt) => {
    const size = parseInt(opt.value, 10);
    if (!Number.isFinite(size)) return;
    opt.textContent = t('token.perPage', { size });
  });
}

function updatePaginationControls(totalCount, totalPages) {
  const info = byId('pagination-info');
  const prevBtn = byId('page-prev');
  const nextBtn = byId('page-next');
  const sizeSelect = byId('page-size');

  refreshPageSizeOptionsI18n();

  if (sizeSelect && String(sizeSelect.value) !== String(pageSize)) {
    sizeSelect.value = String(pageSize);
  }

  if (info) {
    info.textContent = t('token.pagination', { current: totalCount === 0 ? 0 : currentPage, total: totalPages, count: totalCount });
  }
  if (prevBtn) prevBtn.disabled = totalCount === 0 || currentPage <= 1;
  if (nextBtn) nextBtn.disabled = totalCount === 0 || currentPage >= totalPages;
}

function goPrevPage() {
  if (currentPage <= 1) return;
  currentPage -= 1;
  closeSelectAllMenu();
  loadData();
}

function goNextPage() {
  if (currentPage >= currentTotalPages) return;
  currentPage += 1;
  closeSelectAllMenu();
  loadData();
}

function changePageSize() {
  const sizeSelect = byId('page-size');
  const value = sizeSelect ? parseInt(sizeSelect.value, 10) : 0;
  if (!value || value === pageSize) return;
  pageSize = value;
  currentPage = 1;
  closeSelectAllMenu();
  loadData();
}

// ========== NSFW 批量开启 ==========

async function batchEnableNSFW() {
  if (isBatchProcessing) {
    showToast(t('common.taskInProgress'), 'info');
    return;
  }

  const selected = getSelectedTokenRefs();
  const targetCount = selected.length;
  if (targetCount === 0) {
    showToast(t('common.noTokenSelected'), 'error');
    return;
  }
  const msg = t('token.nsfwConfirm', { count: targetCount });

  const ok = await confirmAction(msg, { okText: t('token.nsfwEnable') });
  if (!ok) return;

  // 禁用按钮
  const btn = byId('btn-batch-nsfw');
  if (btn) btn.disabled = true;

  isBatchProcessing = true;
  currentBatchAction = 'nsfw';
  batchTotal = targetCount;
  batchProcessed = 0;
  updateBatchProgress();
  setActionButtonsState();

  try {
    const tokens = selected.length > 0 ? selected.slice() : null;
    const res = await fetch('/v1/admin/tokens/nsfw/enable/async', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        ...buildAuthHeaders(apiKey)
      },
      body: JSON.stringify({ tokens })
    });

    const data = await readJsonResponse(res);
    if (!res.ok) {
      const detail = data && (data.detail || data.message);
      throw new Error(detail || `HTTP ${res.status}`);
    }
    if (!data) {
      throw new Error(t('token.emptyResponse', { status: res.status }));
    }
    if (data.status !== 'success') {
      throw new Error(data.detail || t('common.requestFailed'));
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
          finishBatchProcess(false, { silent: true });
          const summary = msg.result && msg.result.summary ? msg.result.summary : null;
          const okCount = summary ? summary.ok : 0;
          const failCount = summary ? summary.fail : 0;
          let text = t('token.nsfwResult', { ok: okCount, fail: failCount });
          if (msg.warning) text += `\n⚠️ ${msg.warning}`;
          showToast(text, failCount > 0 || msg.warning ? 'warning' : 'success');
          currentBatchTaskId = null;
          BatchSSE.close(batchEventSource);
          batchEventSource = null;
          if (btn) btn.disabled = false;
          setActionButtonsState();
        } else if (msg.type === 'cancelled') {
          finishBatchProcess(true, { silent: true });
          showToast(t('token.stopNsfw'), 'info');
          currentBatchTaskId = null;
          BatchSSE.close(batchEventSource);
          batchEventSource = null;
          if (btn) btn.disabled = false;
          setActionButtonsState();
        } else if (msg.type === 'error') {
          finishBatchProcess(true, { silent: true });
          showToast(t('token.nsfwFailed', { msg: msg.error || t('common.unknownError') }), 'error');
          currentBatchTaskId = null;
          BatchSSE.close(batchEventSource);
          batchEventSource = null;
          if (btn) btn.disabled = false;
          setActionButtonsState();
        }
      },
      onError: () => {
        finishBatchProcess(true, { silent: true });
        showToast(t('common.connectionInterrupted'), 'error');
        currentBatchTaskId = null;
        BatchSSE.close(batchEventSource);
        batchEventSource = null;
        if (btn) btn.disabled = false;
        setActionButtonsState();
      }
    });
  } catch (e) {
    finishBatchProcess(true, { silent: true });
    showToast(t('token.requestError') + ': ' + e.message, 'error');
    if (btn) btn.disabled = false;
    setActionButtonsState();
  }
}



runWhenDomReady(init);
