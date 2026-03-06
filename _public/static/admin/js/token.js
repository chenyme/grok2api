let apiKey = '';
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

const byId = (id) => document.getElementById(id);
const qsa = (selector) => document.querySelectorAll(selector);
const DEFAULT_QUOTA_BASIC = 80;
const DEFAULT_QUOTA_SUPER = 140;

function getDefaultQuotaForPool(pool) {
  return pool === 'ssoSuper' ? DEFAULT_QUOTA_SUPER : DEFAULT_QUOTA_BASIC;
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

function getSelectedTokens() {
  return flatTokens.filter(t => t._selected);
}

function countSelected(tokens) {
  let count = 0;
  for (const t of tokens) {
    if (t._selected) count++;
  }
  return count;
}

function setSelectedForTokens(tokens, selected) {
  tokens.forEach(t => {
    t._selected = selected;
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

function getPaginationData() {
  const filteredTokens = getFilteredTokens();
  const totalCount = filteredTokens.length;
  const totalPages = Math.max(1, Math.ceil(totalCount / pageSize));
  if (currentPage > totalPages) currentPage = totalPages;
  const startIndex = (currentPage - 1) * pageSize;
  const visibleTokens = filteredTokens.slice(startIndex, startIndex + pageSize);
  return { filteredTokens, totalCount, totalPages, visibleTokens };
}

async function init() {
  apiKey = await ensureAdminKey();
  if (apiKey === null) return;
  setupEditPoolDefaults();
  setupConfirmDialog();
  loadData();
}

async function loadData() {
  try {
    const res = await fetch('/v1/admin/tokens', {
      headers: buildAuthHeaders(apiKey)
    });
    if (res.ok) {
      const data = await res.json();
      allTokens = data;
      processTokens(data);
      updateStats(data);
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

// Convert pool dict to flattened array
function processTokens(data) {
  flatTokens = [];
  Object.keys(data).forEach(pool => {
    const tokens = data[pool];
    if (Array.isArray(tokens)) {
      tokens.forEach(t => {
        // Normalize
        const tObj = typeof t === 'string'
          ? { token: t, status: 'active', quota: 0, note: '', use_count: 0, tags: [] }
          : {
            token: t.token,
            status: t.status || 'active',
            quota: t.quota || 0,
            note: t.note || '',
            fail_count: t.fail_count || 0,
            use_count: t.use_count || 0,
            tags: t.tags || [],
            created_at: t.created_at,
            last_used_at: t.last_used_at,
            last_fail_at: t.last_fail_at,
            last_fail_reason: t.last_fail_reason,
            last_sync_at: t.last_sync_at,
            last_asset_clear_at: t.last_asset_clear_at
          };
        flatTokens.push({ ...tObj, pool: pool, _selected: false });
      });
    }
  });
}

function updateStats(data) {
  // Logic same as before, simplified reuse if possible, but let's re-run on flatTokens
  let totalTokens = flatTokens.length;
  let activeTokens = 0;
  let coolingTokens = 0;
  let invalidTokens = 0;
  let nsfwTokens = 0;
  let noNsfwTokens = 0;
  let chatQuota = 0;
  let totalCalls = 0;

  flatTokens.forEach(t => {
    if (t.status === 'active') {
      activeTokens++;
      chatQuota += t.quota;
    } else if (t.status === 'cooling') {
      coolingTokens++;
    } else {
      invalidTokens++;
    }
    if (t.tags && t.tags.includes('nsfw')) {
      nsfwTokens++;
    } else {
      noNsfwTokens++;
    }
    totalCalls += Number(t.use_count || 0);
  });

  const imageQuota = Math.floor(chatQuota / 2);

  setText('stat-total', totalTokens.toLocaleString());
  setText('stat-active', activeTokens.toLocaleString());
  setText('stat-cooling', coolingTokens.toLocaleString());
  setText('stat-invalid', invalidTokens.toLocaleString());

  setText('stat-chat-quota', chatQuota.toLocaleString());
  setText('stat-image-quota', imageQuota.toLocaleString());
  setText('stat-total-calls', totalCalls.toLocaleString());

  updateTabCounts({
    all: totalTokens,
    active: activeTokens,
    cooling: coolingTokens,
    expired: invalidTokens,
    nsfw: nsfwTokens,
    'no-nsfw': noNsfwTokens
  });
}

function renderTable() {
  const tbody = byId('token-table-body');
  const loading = byId('loading');
  const emptyState = byId('empty-state');

  if (loading) loading.classList.add('hidden');

  // 获取筛选后的列表
  const { totalCount, totalPages, visibleTokens } = getPaginationData();
  const indexByRef = new Map(flatTokens.map((t, i) => [t, i]));

  updatePaginationControls(totalCount, totalPages);

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
  visibleTokens.forEach((item) => {
    // 获取原始索引用于操作
    const originalIndex = indexByRef.get(item);
    const tr = document.createElement('tr');
    tr.dataset.index = originalIndex;
    if (item._selected) tr.classList.add('row-selected');

    // Checkbox (Center)
    const tdCheck = document.createElement('td');
    tdCheck.className = 'text-center';
    tdCheck.innerHTML = `<input type="checkbox" class="checkbox" ${item._selected ? 'checked' : ''} onchange="toggleSelect(${originalIndex})">`;

    // Token (Left)
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

    // Type (Center)
    const tdType = document.createElement('td');
    tdType.className = 'text-center';
    tdType.innerHTML = `<span class="badge badge-gray">${escapeHtml(item.pool)}</span>`;

    // Status (Center) - 显示状态和 nsfw 标签
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

    // Quota (Center)
    const tdQuota = document.createElement('td');
    tdQuota.className = 'text-center font-mono text-xs';
    tdQuota.innerText = item.quota;

    // Note (Left)
    const tdNote = document.createElement('td');
    tdNote.className = 'text-left text-gray-500 text-xs token-note-cell';
    tdNote.innerText = item.note || '-';

    // Actions (Center)
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
                     <button onclick="refreshStatus('${item.token}')" class="p-1 text-gray-400 hover:text-black rounded" title="${t('token.refreshStatus')}">
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

function selectAllFiltered() {
  const filtered = getFilteredTokens();
  if (filtered.length === 0) return;
  setSelectedForTokens(filtered, true);
  syncVisibleSelectionUI(true);
  updateSelectionState();
}

function selectVisibleAll() {
  const visible = getVisibleTokens();
  if (visible.length === 0) return;
  setSelectedForTokens(visible, true);
  syncVisibleSelectionUI(true);
  updateSelectionState();
}

function clearAllSelection() {
  if (flatTokens.length === 0) return;
  setSelectedForTokens(flatTokens, false);
  syncVisibleSelectionUI(false);
  updateSelectionState();
}

function toggleSelect(index) {
  flatTokens[index]._selected = !flatTokens[index]._selected;
  const row = document.querySelector(`#token-table-body tr[data-index="${index}"]`);
  if (row) row.classList.toggle('row-selected', flatTokens[index]._selected);
  updateSelectionState();
}

function updateSelectionState() {
  const selectedCount = countSelected(flatTokens);
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
  const content = selected.map(t => t.token).join('\n') + '\n';
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
    byId('edit-quota').value = item.quota;
    byId('edit-note').value = item.note;
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
  // Collect data
  let token;
  const newPool = byId('edit-pool').value.trim();
  const newQuota = parseInt(byId('edit-quota').value) || 0;
  const newNote = byId('edit-note').value.trim().slice(0, 50);

  if (currentEditIndex >= 0) {
    // Updating existing
    const item = flatTokens[currentEditIndex];
    token = item.token;

    // Update flatTokens first to reflect UI
    item.pool = newPool || 'ssoBasic';
    item.quota = newQuota;
    item.note = newNote;
  } else {
    // Creating new
    token = byId('edit-token-display').value.trim();
    if (!token) return showToast(t('token.tokenEmpty'), 'error');

    // Check if exists
    if (flatTokens.some(t => t.token === token)) {
      return showToast(t('token.tokenExists'), 'error');
    }

    flatTokens.push({
      token: token,
      pool: newPool || 'ssoBasic',
      quota: newQuota,
      note: newNote,
      status: 'active', // default
      use_count: 0,
      _selected: false
    });
  }

  await syncToServer();
  closeEditModal();
  // Reload to ensure consistent state/grouping
  // Or simpler: just re-render but syncToServer does the hard work
  loadData();
}

async function deleteToken(index) {
  const ok = await confirmAction(t('token.confirmDelete'), { okText: t('common.delete') });
  if (!ok) return;
  flatTokens.splice(index, 1);
  syncToServer().then(loadData);
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
  item.status = targetStatus;
  await syncToServer();
  await loadData();
  showToast(toDisabled ? t('token.disableDone') : t('token.enableDone'), 'success');
}

function batchDelete() {
  startBatchDelete();
}

function _getBatchStatusTargets(targetStatus) {
  const selected = getSelectedTokens();
  if (selected.length === 0) return { selected, targets: [] };
  const targets = selected.filter(item => item.status !== targetStatus);
  return { selected, targets };
}

async function batchSetStatus(targetStatus) {
  if (isBatchProcessing) {
    showToast(t('common.taskInProgress'), 'info');
    return;
  }
  const { selected, targets } = _getBatchStatusTargets(targetStatus);
  if (selected.length === 0) {
    showToast(t('common.noTokenSelected'), 'error');
    return;
  }
  const toDisabled = targetStatus === 'disabled';
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
  await syncToServer();
  await loadData();
  showToast(toDisabled ? t('token.batchDisableDone') : t('token.batchEnableDone'), 'success');
}

async function batchDisableTokens() {
  await batchSetStatus('disabled');
}

async function batchEnableTokens() {
  await batchSetStatus('active');
}

// Reconstruct object structure and save
async function syncToServer() {
  const newTokens = {};
  flatTokens.forEach(t => {
    if (!newTokens[t.pool]) newTokens[t.pool] = [];
    const payload = {
      token: t.token,
      status: t.status,
      quota: t.quota,
      note: t.note,
      fail_count: t.fail_count,
      use_count: t.use_count || 0,
      tags: Array.isArray(t.tags) ? t.tags : []
    };
    if (typeof t.created_at === 'number') payload.created_at = t.created_at;
    if (typeof t.last_used_at === 'number') payload.last_used_at = t.last_used_at;
    if (typeof t.last_fail_at === 'number') payload.last_fail_at = t.last_fail_at;
    if (typeof t.last_sync_at === 'number') payload.last_sync_at = t.last_sync_at;
    if (typeof t.last_asset_clear_at === 'number') payload.last_asset_clear_at = t.last_asset_clear_at;
    if (typeof t.last_fail_reason === 'string' && t.last_fail_reason) payload.last_fail_reason = t.last_fail_reason;
    newTokens[t.pool].push(payload);
  });

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

  lines.forEach(line => {
    const t = line.trim();
    if (t && !flatTokens.some(ft => ft.token === t)) {
      flatTokens.push({
        token: t,
        pool: pool,
        status: 'active',
        quota: defaultQuota,
        note: '',
        tags: [],
        fail_count: 0,
        use_count: 0,
        _selected: false
      });
    }
  });

  await syncToServer();
  closeImportModal();
  loadData();
}

// Export Logic
function exportTokens() {
  if (flatTokens.length === 0) return showToast(t('token.listEmpty'), 'error');
  const content = flatTokens.map(t => t.token).join('\n') + '\n';
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

async function refreshStatus(token) {
  try {
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
      body: JSON.stringify({ token: token })
    });

    const data = await res.json();

    if (res.ok && data.status === 'success') {
      const isSuccess = data.results && data.results[token];
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

  const selected = getSelectedTokens();
  if (selected.length === 0) return showToast(t('common.noTokenSelected'), 'error');

  // Init state
  isBatchProcessing = true;
  isBatchPaused = false;
  currentBatchAction = 'refresh';
  batchQueue = selected.map(t => t.token);
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
    count = countSelected(flatTokens);
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
  const selected = getSelectedTokens();
  if (selected.length === 0) return showToast(t('common.noTokenSelected'), 'error');
  const ok = await confirmAction(t('token.confirmBatchDelete', { count: selected.length }), { okText: t('common.delete') });
  if (!ok) return;

  isBatchProcessing = true;
  isBatchPaused = false;
  currentBatchAction = 'delete';
  batchQueue = selected.map(t => t.token);
  batchTotal = batchQueue.length;
  batchProcessed = 0;

  updateBatchProgress();
  setActionButtonsState();

  try {
    const toRemove = new Set(batchQueue);
    flatTokens = flatTokens.filter(t => !toRemove.has(t.token));
    await syncToServer();
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

  // 更新 Tab 样式和 ARIA
  document.querySelectorAll('.tab-item').forEach(tab => {
    const isActive = tab.dataset.filter === status;
    tab.classList.toggle('active', isActive);
    tab.setAttribute('aria-selected', isActive ? 'true' : 'false');
  });

  renderTable();
}

function getFilteredTokens() {
  if (currentFilter === 'all') return flatTokens;

  return flatTokens.filter(t => {
    if (currentFilter === 'active') return t.status === 'active';
    if (currentFilter === 'cooling') return t.status === 'cooling';
    if (currentFilter === 'expired') return t.status !== 'active' && t.status !== 'cooling';
    if (currentFilter === 'nsfw') return t.tags && t.tags.includes('nsfw');
    if (currentFilter === 'no-nsfw') return !t.tags || !t.tags.includes('nsfw');
    return true;
  });
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
  return getPaginationData().visibleTokens;
}

function updatePaginationControls(totalCount, totalPages) {
  const info = byId('pagination-info');
  const prevBtn = byId('page-prev');
  const nextBtn = byId('page-next');
  const sizeSelect = byId('page-size');

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
  renderTable();
}

function goNextPage() {
  const totalCount = getFilteredTokens().length;
  const totalPages = Math.max(1, Math.ceil(totalCount / pageSize));
  if (currentPage >= totalPages) return;
  currentPage += 1;
  renderTable();
}

function changePageSize() {
  const sizeSelect = byId('page-size');
  const value = sizeSelect ? parseInt(sizeSelect.value, 10) : 0;
  if (!value || value === pageSize) return;
  pageSize = value;
  currentPage = 1;
  renderTable();
}

// ========== NSFW 批量开启 ==========

async function batchEnableNSFW() {
  if (isBatchProcessing) {
    showToast(t('common.taskInProgress'), 'info');
    return;
  }

  const selected = getSelectedTokens();
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
    const tokens = selected.length > 0 ? selected.map(t => t.token) : null;
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



window.onload = init;
