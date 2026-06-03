/* Grok2API - model registry admin page */

let lastRegistryModels = [];
const selectedRemoteModelIds = new Set();

function modelEsc(value) {
  return String(value || '')
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}

function modelFmtTs(ts) {
  const n = Number(ts || 0);
  if (!n) return '-';
  return new Date(n * 1000).toLocaleString();
}

async function modelApi(path, options = {}) {
  const key = await adminKey.get();
  const res = await fetch(ADMIN_API + path, {
    ...options,
    headers: {
      ...(options.body != null && { 'Content-Type': 'application/json' }),
      Authorization: `Bearer ${key}`,
      ...(options.headers || {}),
    },
  });
  if (res.status === 401) {
    adminLogout();
    return null;
  }
  return res;
}

function renderRegistry(data) {
  const body = document.getElementById('models-body');
  const meta = document.getElementById('registry-meta');
  const aliasList = document.getElementById('alias-list');
  if (!body || !meta) return;

  const models = Array.isArray(data.models) ? data.models : [];
  const manualModels = Array.isArray(data.manual_models) ? data.manual_models : [];
  lastRegistryModels = models;

  meta.textContent = `enabled=${!!data.enabled} · source=${data.source || '-'} · last_sync=${modelFmtTs(data.last_sync_at)} · remote=${data.remote_count || 0} · builtin=${data.supported_count || 0} · manual=${manualModels.length} · selected=${selectedRemoteModelIds.size}`;

  if (aliasList) {
    aliasList.innerHTML = manualModels.length
      ? manualModels.map((m) => {
          const mid = String(m.id || '');
          return `<div><span class="mono">${modelEsc(mid)}</span> : ${modelEsc(m.name || mid)}</div>`;
        }).join('')
      : '<div>暂无手工模型</div>';
  }

  if (!models.length) {
    body.innerHTML = '<tr><td colspan="6" style="text-align:center;color:#aaa;padding:28px">暂无模型数据</td></tr>';
    return;
  }

  body.innerHTML = models.map((m) => {
    const id = String(m.id || '');
    const checked = selectedRemoteModelIds.has(id) ? 'checked' : '';
    const mapped = m.mapped_to ? `<span class="badge">${modelEsc(m.mapped_to)}</span>` : '-';
    return `
      <tr>
        <td><input type="checkbox" class="remote-model-checkbox" data-model-id="${modelEsc(id)}" ${checked}></td>
        <td class="mono">${modelEsc(id)}</td>
        <td>${modelEsc(m.owned_by || m.source || '-')}</td>
        <td>${m.supported ? '<span class="badge yes">是</span>' : '<span class="badge no">否</span>'}</td>
        <td>${m.executable ? '<span class="badge yes">是</span>' : '<span class="badge no">否</span>'}</td>
        <td>${mapped}</td>
      </tr>`;
  }).join('');

  body.querySelectorAll('.remote-model-checkbox').forEach((el) => {
    el.addEventListener('change', () => {
      const modelId = String(el.getAttribute('data-model-id') || '').trim();
      if (!modelId) return;
      if (el.checked) selectedRemoteModelIds.add(modelId);
      else selectedRemoteModelIds.delete(modelId);
      meta.textContent = `enabled=${!!data.enabled} · source=${data.source || '-'} · last_sync=${modelFmtTs(data.last_sync_at)} · remote=${data.remote_count || 0} · builtin=${data.supported_count || 0} · manual=${manualModels.length} · selected=${selectedRemoteModelIds.size}`;
    });
  });
}

async function loadRegistry() {
  const res = await modelApi('/models/registry', { cache: 'no-store' });
  if (!res) return;
  if (!res.ok) throw new Error(`HTTP ${res.status}`);
  renderRegistry(await res.json());
}

async function discoverRegistry() {
  const status = document.getElementById('sync-status');
  if (status) status.textContent = '正在同步公开模型...';
  const res = await modelApi('/models/registry/discover', {
    method: 'POST',
    body: JSON.stringify({}),
  });
  if (!res) return;
  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    if (status) status.textContent = '';
    showToast(data.detail || `同步失败: HTTP ${res.status}`, 'error');
    return;
  }
  if (status) status.textContent = `同步成功：remote=${data.remote_count || 0}`;
  showToast('模型同步成功', 'success');
  await loadRegistry();
}

async function saveManual() {
  const modelId = (document.getElementById('manual-model-id')?.value || '').trim();
  const modelName = (document.getElementById('manual-model-name')?.value || '').trim() || modelId;
  if (!modelId) {
    showToast('模型 ID 不能为空', 'error');
    return;
  }
  const payload = { id: modelId, name: modelName };

  const res = await modelApi('/models/registry/manual/upsert', {
    method: 'POST',
    body: JSON.stringify(payload),
  });
  if (!res) return;
  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    showToast(data.detail || `保存失败: HTTP ${res.status}`, 'error');
    return;
  }
  showToast('手工模型已加入下拉', 'success');
  const inputId = document.getElementById('manual-model-id');
  const inputName = document.getElementById('manual-model-name');
  if (inputId) inputId.value = '';
  if (inputName) inputName.value = '';
  await loadRegistry();
}

async function deleteManual() {
  const modelId = (document.getElementById('manual-model-id')?.value || '').trim();
  if (!modelId) {
    showToast('请填写要删除的模型 ID', 'error');
    return;
  }
  const res = await modelApi('/models/registry/manual/delete', {
    method: 'POST',
    body: JSON.stringify({ id: modelId }),
  });
  if (!res) return;
  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    showToast(data.detail || `删除失败: HTTP ${res.status}`, 'error');
    return;
  }
  showToast('手工模型已删除', 'success');
  await loadRegistry();
}

async function selectAllVisible() {
  lastRegistryModels.forEach((m) => {
    const id = String((m && m.id) || '').trim();
    if (id) selectedRemoteModelIds.add(id);
  });
  await loadRegistry();
}

async function clearSelection() {
  selectedRemoteModelIds.clear();
  await loadRegistry();
}

async function batchAddSelected() {
  const ids = [...selectedRemoteModelIds];
  if (!ids.length) {
    showToast('请先勾选至少一个模型', 'error');
    return;
  }

  let ok = 0;
  let failed = 0;
  for (const id of ids) {
    const payload = { id, name: id };
    const res = await modelApi('/models/registry/manual/upsert', {
      method: 'POST',
      body: JSON.stringify(payload),
    });
    if (res && res.ok) ok += 1;
    else failed += 1;
  }
  showToast(`批量添加完成：成功 ${ok}，失败 ${failed}`, failed ? 'error' : 'success');
  await loadRegistry();
}

async function setRegistryEnabled(enabled) {
  const res = await modelApi(`/models/registry/${enabled ? 'enable' : 'disable'}`, {
    method: 'POST',
  });
  if (!res) return;
  if (!res.ok) {
    showToast(`${enabled ? '启用' : '停用'}失败: HTTP ${res.status}`, 'error');
    return;
  }
  showToast(enabled ? '已启用模型 overlay' : '已停用模型 overlay', 'success');
  await loadRegistry();
}

function initModelRegistryPage() {
  document.getElementById('discover-btn')?.addEventListener('click', discoverRegistry);
  document.getElementById('discover-panel-btn')?.addEventListener('click', discoverRegistry);
  document.getElementById('enable-btn')?.addEventListener('click', () => setRegistryEnabled(true));
  document.getElementById('disable-btn')?.addEventListener('click', () => setRegistryEnabled(false));
  document.getElementById('save-manual-btn')?.addEventListener('click', saveManual);
  document.getElementById('delete-manual-btn')?.addEventListener('click', deleteManual);
  document.getElementById('select-all-btn')?.addEventListener('click', selectAllVisible);
  document.getElementById('clear-selection-btn')?.addEventListener('click', clearSelection);
  document.getElementById('batch-add-btn')?.addEventListener('click', batchAddSelected);
  loadRegistry().catch((err) => showToast(`加载失败: ${err.message}`, 'error'));
}
