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
const BATCH_SIZE = 50;

async function init() {
  apiKey = await ensureApiKey();
  if (apiKey === null) return;
  setupConfirmDialog();
  loadData();
}

async function loadData() {
  try {
    const res = await fetch('/api/v1/admin/tokens', {
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
    showToast('加载失败: ' + e.message, 'error');
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
          ? { token: t, status: 'active', quota: 0, note: '', use_count: 0 }
          : {
            token: t.token,
            status: t.status || 'active',
            quota: t.quota || 0,
            note: t.note || '',
            fail_count: t.fail_count || 0,
            use_count: t.use_count || 0
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
    totalCalls += Number(t.use_count || 0);
  });

  const imageQuota = Math.floor(chatQuota / 2);

  const setText = (id, text) => {
    const el = document.getElementById(id);
    if (el) el.innerText = text;
  };

  setText('stat-total', totalTokens.toLocaleString());
  setText('stat-active', activeTokens.toLocaleString());
  setText('stat-cooling', coolingTokens.toLocaleString());
  setText('stat-invalid', invalidTokens.toLocaleString());

  setText('stat-chat-quota', chatQuota.toLocaleString());
  setText('stat-image-quota', imageQuota.toLocaleString());
  setText('stat-total-calls', totalCalls.toLocaleString());
}

function renderTable() {
  const tbody = document.getElementById('token-table-body');
  const loading = document.getElementById('loading');
  const emptyState = document.getElementById('empty-state');

  tbody.innerHTML = '';
  loading.classList.add('hidden');

  // 更新 Tab 计数
  updateTabCounts();

  // 获取筛选后的列表
  const filteredTokens = getFilteredTokens();

  if (filteredTokens.length === 0) {
    emptyState.classList.remove('hidden');
    return;
  }
  emptyState.classList.add('hidden');

  filteredTokens.forEach((item) => {
    // 获取原始索引用于操作
    const originalIndex = flatTokens.indexOf(item);
    const tr = document.createElement('tr');

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

    // Status (Center)
    const tdStatus = document.createElement('td');
    let statusClass = 'badge-gray';
    if (item.status === 'active') statusClass = 'badge-green';
    else if (item.status === 'cooling') statusClass = 'badge-orange';
    else statusClass = 'badge-red';
    tdStatus.className = 'text-center';
    tdStatus.innerHTML = `<span class="badge ${statusClass}">${item.status}</span>`;

    // Quota (Center)
    const tdQuota = document.createElement('td');
    tdQuota.className = 'text-center font-mono text-xs';
    tdQuota.innerText = item.quota;

    // Note (Left)
    const tdNote = document.createElement('td');
    tdNote.className = 'text-left text-gray-500 text-xs truncate max-w-[150px]';
    tdNote.innerText = item.note || '-';

    // Actions (Center)
    const tdActions = document.createElement('td');
    tdActions.className = 'text-center';
    tdActions.innerHTML = `
                <div class="flex items-center justify-center gap-2">
                     <button onclick="refreshStatus('${item.token}')" class="p-1 text-gray-400 hover:text-black rounded" title="刷新状态">
                        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="23 4 23 10 17 10"></polyline><polyline points="1 20 1 14 7 14"></polyline><path d="M3.51 9a9 9 0 0 1 14.85-3.36L23 10M1 14l4.64 4.36A9 9 0 0 0 20.49 15"></path></svg>
                     </button>
                     <button onclick="openEditModal(${originalIndex})" class="p-1 text-gray-400 hover:text-black rounded" title="编辑">
                        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M11 4H4a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-7"></path><path d="M18.5 2.5a2.121 2.121 0 0 1 3 3L12 15l-4 1 1-4 9.5-9.5z"></path></svg>
                     </button>
                     <button onclick="deleteToken(${originalIndex})" class="p-1 text-gray-400 hover:text-red-600 rounded" title="删除">
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

    tbody.appendChild(tr);
  });

  updateSelectionState();
}

// Selection Logic
function toggleSelectAll() {
  const checkbox = document.getElementById('select-all');
  const checked = checkbox.checked;
  // 只选择当前筛选后可见的 Token
  getFilteredTokens().forEach(t => t._selected = checked);
  renderTable();
}

function toggleSelect(index) {
  flatTokens[index]._selected = !flatTokens[index]._selected;
  updateSelectionState();
}

function updateSelectionState() {
  const selectedCount = flatTokens.filter(t => t._selected).length;
  const allSelected = flatTokens.length > 0 && selectedCount === flatTokens.length;

  document.getElementById('select-all').checked = allSelected;
  document.getElementById('selected-count').innerText = selectedCount;
  setActionButtonsState();
}

// Actions
function addToken() {
  openEditModal(-1);
}

// Batch export (Selected only)
function batchExport() {
  const selected = flatTokens.filter(t => t._selected);
  if (selected.length === 0) return showToast("未选择 Token", 'error');
  let content = "";
  selected.forEach(t => content += t.token + "\n");
  const blob = new Blob([content], { type: 'text/plain' });
  const url = window.URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = `tokens_export_selected_${new Date().toISOString().slice(0, 10)}.txt`;
  document.body.appendChild(a);
  a.click();
  window.URL.revokeObjectURL(url);
  document.body.removeChild(a);
}


// Modal Logic
let currentEditIndex = -1;
function openEditModal(index) {
  const modal = document.getElementById('edit-modal');
  if (!modal) return;

  currentEditIndex = index;

  if (index >= 0) {
    // Edit existing
    const item = flatTokens[index];
    document.getElementById('edit-token-display').value = item.token;
    document.getElementById('edit-original-token').value = item.token;
    document.getElementById('edit-original-pool').value = item.pool;
    document.getElementById('edit-pool').value = item.pool;
    document.getElementById('edit-quota').value = item.quota;
    document.getElementById('edit-note').value = item.note;
    document.querySelector('#edit-modal h3').innerText = '编辑 Token';
  } else {
    // New Token
    document.getElementById('edit-token-display').value = '';
    document.getElementById('edit-token-display').disabled = false;
    document.getElementById('edit-token-display').placeholder = 'sk-...';
    document.getElementById('edit-token-display').classList.remove('bg-gray-50', 'text-gray-500');

    document.getElementById('edit-original-token').value = '';
    document.getElementById('edit-original-pool').value = '';
    document.getElementById('edit-pool').value = 'ssoBasic';
    document.getElementById('edit-quota').value = 80;
    document.getElementById('edit-note').value = '';
    document.querySelector('#edit-modal h3').innerText = '添加 Token';
  }

  modal.classList.remove('hidden');
  requestAnimationFrame(() => {
    modal.classList.add('is-open');
  });
}

function closeEditModal() {
  const modal = document.getElementById('edit-modal');
  if (!modal) return;
  modal.classList.remove('is-open');
  setTimeout(() => {
    modal.classList.add('hidden');
    // reset styles for token input
    const input = document.getElementById('edit-token-display');
    if (input) {
      input.disabled = true;
      input.classList.add('bg-gray-50', 'text-gray-500');
    }
  }, 200);
}

async function saveEdit() {
  // Collect data
  let token, pool, quota, note;
  const newPool = document.getElementById('edit-pool').value.trim();
  const newQuota = parseInt(document.getElementById('edit-quota').value) || 0;
  const newNote = document.getElementById('edit-note').value.trim().slice(0, 50);

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
    token = document.getElementById('edit-token-display').value.trim();
    if (!token) return showToast('Token 不能为空', 'error');

    // Check if exists
    if (flatTokens.some(t => t.token === token)) {
      return showToast('Token 已存在', 'error');
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
  const ok = await confirmAction('确定要删除此 Token 吗？', { okText: '删除' });
  if (!ok) return;
  flatTokens.splice(index, 1);
  syncToServer().then(loadData);
}

function batchDelete() {
  startBatchDelete();
}

// Reconstruct object structure and save
async function syncToServer() {
  const newTokens = {};
  flatTokens.forEach(t => {
    if (!newTokens[t.pool]) newTokens[t.pool] = [];
    newTokens[t.pool].push({
      token: t.token,
      status: t.status,
      quota: t.quota,
      note: t.note,
      fail_count: t.fail_count,
      use_count: t.use_count || 0
    });
  });

  try {
    const res = await fetch('/api/v1/admin/tokens', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        ...buildAuthHeaders(apiKey)
      },
      body: JSON.stringify(newTokens)
    });
    if (!res.ok) showToast('保存失败', 'error');
  } catch (e) {
    showToast('保存错误: ' + e.message, 'error');
  }
}

// Import Logic
function openImportModal() {
  const modal = document.getElementById('import-modal');
  if (!modal) return;
  modal.classList.remove('hidden');
  requestAnimationFrame(() => {
    modal.classList.add('is-open');
  });
}

function closeImportModal() {
  const modal = document.getElementById('import-modal');
  if (!modal) return;
  modal.classList.remove('is-open');
  setTimeout(() => {
    modal.classList.add('hidden');
    const input = document.getElementById('import-text');
    if (input) input.value = '';
  }, 200);
}

async function submitImport() {
  const pool = document.getElementById('import-pool').value.trim() || 'ssoBasic';
  const text = document.getElementById('import-text').value;
  const lines = text.split('\n');

  lines.forEach(line => {
    const t = line.trim();
    if (t && !flatTokens.some(ft => ft.token === t)) {
      flatTokens.push({
        token: t,
        pool: pool,
        status: 'active',
        quota: 80,
        note: '',
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
  let content = "";
  flatTokens.forEach(t => content += t.token + "\n");
  if (!content) return showToast("列表为空", 'error');

  const blob = new Blob([content], { type: 'text/plain' });
  const url = window.URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = `tokens_export_${new Date().toISOString().slice(0, 10)}.txt`;
  document.body.appendChild(a);
  a.click();
  window.URL.revokeObjectURL(url);
  document.body.removeChild(a);
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

    const res = await fetch('/api/v1/admin/tokens/refresh', {
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
        showToast('刷新成功', 'success');
      } else {
        showToast('刷新失败', 'error');
      }
    } else {
      showToast('刷新失败', 'error');
    }
  } catch (e) {
    console.error(e);
    showToast('请求错误', 'error');
  }
}

async function startBatchRefresh() {
  if (isBatchProcessing) {
    showToast('当前有任务进行中', 'info');
    return;
  }

  const selected = flatTokens.filter(t => t._selected);
  if (selected.length === 0) return showToast("未选择 Token", 'error');

  // Init state
  isBatchProcessing = true;
  isBatchPaused = false;
  currentBatchAction = 'refresh';
  batchQueue = selected.map(t => t.token);
  batchTotal = batchQueue.length;
  batchProcessed = 0;

  updateBatchProgress();
  setActionButtonsState();
  processBatchQueue();
}

async function processBatchQueue() {
  if (!isBatchProcessing || isBatchPaused || currentBatchAction !== 'refresh') return;

  if (batchQueue.length === 0) {
    // Done
    finishBatchProcess();
    return;
  }

  // Take chunk
  const chunk = batchQueue.splice(0, BATCH_SIZE);

  try {
    const res = await fetch('/api/v1/admin/tokens/refresh', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        ...buildAuthHeaders(apiKey)
      },
      body: JSON.stringify({ tokens: chunk })
    });

    if (res.ok) {
      batchProcessed += chunk.length;
    } else {
      showToast('部分刷新失败', 'error');
      batchProcessed += chunk.length;
    }
  } catch (e) {
    showToast('网络请求错误', 'error');
    batchProcessed += chunk.length;
  }
  updateBatchProgress();

  // Recursive call for next batch
  // Small delay to allow UI updates and interactions
  if (!isBatchProcessing || isBatchPaused) return;
  setTimeout(() => {
    processBatchQueue();
  }, 400);
}

function toggleBatchPause() {
  if (!isBatchProcessing) return;
  isBatchPaused = !isBatchPaused;
  updateBatchProgress();
  if (!isBatchPaused) {
    if (currentBatchAction === 'refresh') {
      processBatchQueue();
    } else if (currentBatchAction === 'delete') {
      processDeleteQueue();
    }
  }
}

function stopBatchRefresh() {
  if (!isBatchProcessing) return;
  finishBatchProcess(true);
}

function finishBatchProcess(aborted = false) {
  const action = currentBatchAction;
  isBatchProcessing = false;
  isBatchPaused = false;
  batchQueue = [];
  currentBatchAction = null;

  updateBatchProgress();
  setActionButtonsState();
  updateSelectionState();
  loadData(); // Final data refresh

  if (aborted) {
    showToast(action === 'delete' ? '已终止删除' : '已终止刷新', 'info');
  } else {
    showToast(action === 'delete' ? '删除完成' : '刷新完成', 'success');
  }
}

async function batchUpdate() {
  startBatchRefresh();
}

function updateBatchProgress() {
  const container = document.getElementById('batch-progress');
  const text = document.getElementById('batch-progress-text');
  const pauseBtn = document.getElementById('btn-pause-action');
  const stopBtn = document.getElementById('btn-stop-action');
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
    pauseBtn.textContent = isBatchPaused ? '继续' : '暂停';
    pauseBtn.classList.remove('hidden');
  }
  if (stopBtn) stopBtn.classList.remove('hidden');
}

function setActionButtonsState() {
  const selectedCount = flatTokens.filter(t => t._selected).length;
  const disabled = isBatchProcessing;
  const exportBtn = document.getElementById('btn-batch-export');
  const updateBtn = document.getElementById('btn-batch-update');
  const deleteBtn = document.getElementById('btn-batch-delete');
  if (exportBtn) exportBtn.disabled = disabled || selectedCount === 0;
  if (updateBtn) updateBtn.disabled = disabled || selectedCount === 0;
  if (deleteBtn) deleteBtn.disabled = disabled || selectedCount === 0;
}

async function startBatchDelete() {
  if (isBatchProcessing) {
    showToast('当前有任务进行中', 'info');
    return;
  }
  const selected = flatTokens.filter(t => t._selected);
  if (selected.length === 0) return showToast("未选择 Token", 'error');
  const ok = await confirmAction(`确定要删除选中的 ${selected.length} 个 Token 吗？`, { okText: '删除' });
  if (!ok) return;

  isBatchProcessing = true;
  isBatchPaused = false;
  currentBatchAction = 'delete';
  batchQueue = selected.map(t => t.token);
  batchTotal = batchQueue.length;
  batchProcessed = 0;

  updateBatchProgress();
  setActionButtonsState();
  processDeleteQueue();
}

let confirmResolver = null;

function setupConfirmDialog() {
  const dialog = document.getElementById('confirm-dialog');
  if (!dialog) return;
  const okBtn = document.getElementById('confirm-ok');
  const cancelBtn = document.getElementById('confirm-cancel');
  dialog.addEventListener('click', (event) => {
    if (event.target === dialog) {
      closeConfirm(false);
    }
  });
  if (okBtn) okBtn.addEventListener('click', () => closeConfirm(true));
  if (cancelBtn) cancelBtn.addEventListener('click', () => closeConfirm(false));
}

function confirmAction(message, options = {}) {
  const dialog = document.getElementById('confirm-dialog');
  if (!dialog) {
    return Promise.resolve(false);
  }
  const messageEl = document.getElementById('confirm-message');
  const okBtn = document.getElementById('confirm-ok');
  const cancelBtn = document.getElementById('confirm-cancel');
  if (messageEl) messageEl.textContent = message;
  if (okBtn) okBtn.textContent = options.okText || '确定';
  if (cancelBtn) cancelBtn.textContent = options.cancelText || '取消';
  return new Promise(resolve => {
    confirmResolver = resolve;
    dialog.classList.remove('hidden');
    requestAnimationFrame(() => {
      dialog.classList.add('is-open');
    });
  });
}

function closeConfirm(ok) {
  const dialog = document.getElementById('confirm-dialog');
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

async function processDeleteQueue() {
  if (!isBatchProcessing || isBatchPaused || currentBatchAction !== 'delete') return;
  if (batchQueue.length === 0) {
    finishBatchProcess();
    return;
  }
  const chunk = batchQueue.splice(0, BATCH_SIZE);
  const toRemove = new Set(chunk);
  flatTokens = flatTokens.filter(t => !toRemove.has(t.token));
  try {
    await syncToServer();
    batchProcessed += chunk.length;
  } catch (e) {
    showToast('删除失败', 'error');
    batchProcessed += chunk.length;
  }
  updateBatchProgress();
  if (!isBatchProcessing || isBatchPaused) return;
  setTimeout(() => {
    processDeleteQueue();
  }, 400);
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
    return true;
  });
}

function updateTabCounts() {
  const counts = {
    all: flatTokens.length,
    active: flatTokens.filter(t => t.status === 'active').length,
    cooling: flatTokens.filter(t => t.status === 'cooling').length,
    expired: flatTokens.filter(t => t.status !== 'active' && t.status !== 'cooling').length
  };

  Object.entries(counts).forEach(([key, count]) => {
    const el = document.getElementById(`tab-count-${key}`);
    if (el) el.textContent = count;
  });
}

// ========== NSFW 批量开启 ==========

async function batchEnableNSFW() {
  if (isBatchProcessing) {
    showToast('当前有任务进行中', 'info');
    return;
  }

  const selected = flatTokens.filter(t => t._selected);
  const targetCount = selected.length || flatTokens.length;
  const msg = selected.length === 0
    ? `是否对全部 ${targetCount} 个 Token 开启 NSFW 模式？`
    : `是否为选中的 ${selected.length} 个 Token 开启 NSFW 模式？`;

  const ok = await confirmAction(msg, { okText: '开启 NSFW' });
  if (!ok) return;

  // 禁用按钮
  const btn = document.getElementById('btn-batch-nsfw');
  if (btn) btn.disabled = true;

  isBatchProcessing = true;
  currentBatchAction = 'nsfw';
  updateBatchProgress();

  try {
    const tokens = selected.length > 0 ? selected.map(t => t.token) : null;

    const res = await fetch('/api/v1/admin/tokens/nsfw/enable', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        ...buildAuthHeaders(apiKey)
      },
      body: JSON.stringify({ tokens })
    });

    const data = await res.json();

    if (res.ok && data.status === 'success') {
      const { ok: okCount, fail } = data.summary;
      showToast(
        `NSFW 开启完成：成功 ${okCount}，失败 ${fail}`,
        fail > 0 ? 'warning' : 'success'
      );
    } else {
      showToast('开启失败: ' + (data.detail || '未知错误'), 'error');
    }
  } catch (e) {
    showToast('请求错误: ' + e.message, 'error');
  } finally {
    isBatchProcessing = false;
    currentBatchAction = null;
    if (btn) btn.disabled = false;
    updateBatchProgress();
    setActionButtonsState();
  }
}



window.onload = init;
