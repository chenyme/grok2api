(() => {
const IS_SPA = window.__GROK_ADMIN_SPA__ === true;

let keysData = [];
let selectedKeys = new Set();
let keysInitialized = false;
let tableClickHandler = null;

// 性能优化：DOM 元素缓存
const _domCache = new Map();
function getCachedEl(id) {
  if (!_domCache.has(id)) {
    _domCache.set(id, document.getElementById(id));
  }
  return _domCache.get(id);
}

// 性能优化：事件委托初始化
function setupTableEventDelegation() {
  const tbody = document.getElementById("key-table-body");
  if (!tbody) return;

  if (tableClickHandler) {
    tbody.removeEventListener("click", tableClickHandler);
  }

  tableClickHandler = (e) => {
    const target = e.target;
    const btn = target.closest("button");
    const checkbox = target.closest('input[type="checkbox"]');
    const row = target.closest("tr");

    if (!row) return;
    const id = row.dataset.id;
    if (!id) return;

    // 复选框点击
    if (checkbox && checkbox.classList.contains("key-checkbox")) {
      toggleSelect(id);
      return;
    }

    // 按钮点击
    if (btn) {
      const action = btn.dataset.action;
      const key = keysData.find((k) => k.id === id);
      if (!key) return;

      if (action === "copy") {
        copyKey(key.key);
      } else if (action === "toggle") {
        toggleKey(id, !key.enabled);
      } else if (action === "delete") {
        deleteKey(id);
      } else if (action === "edit") {
        openEditModal(id, key.name || "");
      }
    }
  };

  tbody.addEventListener("click", tableClickHandler);
}

async function loadKeys() {
  if (!keysInitialized) return;
  const apiKey = await ensureApiKey();
  if (!apiKey) return;

  try {
    const res = await fetch("/api/v1/admin/keys", {
      headers: buildAuthHeaders(apiKey),
    });

    if (!res.ok) throw new Error("Failed to load keys");

    const data = await res.json();
    if (data.status === "success") {
      keysData = data.data || [];
      renderKeys();
    }
  } catch (e) {
    showToast("加载 Key 列表失败", "error");
  }
}

function renderKeys() {
  if (!keysInitialized) return;
  const tbody = getCachedEl("key-table-body");

  if (!keysData.length) {
    tbody.innerHTML =
      '<tr><td colspan="6" class="table-empty">暂无 API Key</td></tr>';
    return;
  }

  // 性能优化：使用 DocumentFragment + data-action 替代 onclick
  const fragment = document.createDocumentFragment();
  keysData.forEach((key) => {
    const tr = document.createElement("tr");
    const safeId = escapeAttr(key.id || "");
    const safeName = escapeHtml(key.name || "未命名");
    const safeMaskedKey = escapeHtml(maskKey(key.key) || "");
    const safeCreatedAt = escapeHtml(formatDate(key.created_at) || "-");
    const statusText = key.enabled ? "启用" : "禁用";
    tr.dataset.id = key.id;
    tr.innerHTML = `
      <td>
        <input type="checkbox" class="checkbox key-checkbox" data-id="${safeId}"
               ${selectedKeys.has(key.id) ? "checked" : ""}>
      </td>
      <td>
        <div class="key-name">
          <span class="key-name-text">${safeName}</span>
          <button class="key-edit-btn" data-action="edit">
            <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
              <path d="M17 3a2.828 2.828 0 1 1 4 4L7.5 20.5 2 22l1.5-5.5L17 3z"></path>
            </svg>
          </button>
        </div>
      </td>
      <td>
        <span class="key-value">${safeMaskedKey}</span>
      </td>
      <td class="font-mono text-xs">${safeCreatedAt}</td>
      <td>
        <span class="key-status ${key.enabled ? "enabled" : "disabled"}">
          ${statusText}
        </span>
      </td>
      <td>
        <div class="key-actions">
          <button class="key-action-btn" data-action="copy" title="复制">
            <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
              <rect x="9" y="9" width="13" height="13" rx="2" ry="2"></rect>
              <path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"></path>
            </svg>
          </button>
          <button class="key-action-btn" data-action="toggle" title="${key.enabled ? "禁用" : "启用"}">
            <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
              ${
                key.enabled
                  ? '<path d="M18.36 6.64a9 9 0 1 1-12.73 0"></path><line x1="12" y1="2" x2="12" y2="12"></line>'
                  : '<circle cx="12" cy="12" r="10"></circle><polyline points="12 6 12 12 16 14"></polyline>'
              }
            </svg>
          </button>
          <button class="key-action-btn danger" data-action="delete" title="删除">
            <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
              <polyline points="3 6 5 6 21 6"></polyline>
              <path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"></path>
            </svg>
          </button>
        </div>
      </td>
    `;
    fragment.appendChild(tr);
  });

  tbody.replaceChildren(fragment);
  updateBatchActions();
}

function maskKey(key) {
  if (!key || key.length < 12) return key;
  return key.substring(0, 6) + "..." + key.substring(key.length - 4);
}

function formatDate(dateStr) {
  if (!dateStr) return "-";
  const d = new Date(dateStr);
  return d.toLocaleString("zh-CN");
}

function escapeHtml(value) {
  const text = value == null ? "" : String(value);
  return text
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#039;");
}

function escapeAttr(value) {
  return escapeHtml(value).replace(/`/g, "&#096;");
}

function copyKey(key) {
  navigator.clipboard
    .writeText(key)
    .then(() => {
      showToast("已复制到剪贴板", "success");
    })
    .catch(() => {
      showToast("复制失败", "error");
    });
}

function toggleSelect(id) {
  if (selectedKeys.has(id)) {
    selectedKeys.delete(id);
  } else {
    selectedKeys.add(id);
  }
  updateBatchActions();
}

function toggleSelectAll() {
  const selectAll = getCachedEl("select-all");
  if (selectAll.checked) {
    keysData.forEach((key) => selectedKeys.add(key.id));
  } else {
    selectedKeys.clear();
  }
  renderKeys();
}

function updateBatchActions() {
  if (!keysInitialized) return;
  const batchActions = getCachedEl("batch-actions");
  const selectedCount = getCachedEl("selected-count");

  if (selectedKeys.size > 0) {
    batchActions.classList.remove("hidden");
    selectedCount.textContent = selectedKeys.size;
  } else {
    batchActions.classList.add("hidden");
  }
}

// Modal functions
function openAddModal() {
  document.getElementById("add-modal").classList.remove("hidden");
  document.getElementById("add-name").value = "";
  document.getElementById("add-name").focus();
}

function closeAddModal() {
  document.getElementById("add-modal").classList.add("hidden");
}

function openBatchModal() {
  document.getElementById("batch-modal").classList.remove("hidden");
}

function closeBatchModal() {
  document.getElementById("batch-modal").classList.add("hidden");
}

function openEditModal(id, name) {
  document.getElementById("edit-modal").classList.remove("hidden");
  document.getElementById("edit-key-id").value = id;
  document.getElementById("edit-name").value = name;
  document.getElementById("edit-name").focus();
}

function closeEditModal() {
  document.getElementById("edit-modal").classList.add("hidden");
}

// API functions
async function createKey() {
  const apiKey = await ensureApiKey();
  if (!apiKey) return;

  const name = document.getElementById("add-name").value.trim();

  try {
    const res = await fetch("/api/v1/admin/keys", {
      method: "POST",
      headers: {
        ...buildAuthHeaders(apiKey),
        "Content-Type": "application/json",
      },
      body: JSON.stringify({ name }),
    });

    if (res.ok) {
      const data = await res.json();
      showToast("创建成功", "success");
      closeAddModal();
      loadKeys();

      // 显示新 Key
      if (data.data && data.data.key) {
        setTimeout(() => {
          if (
            confirm("新 Key 已创建，是否复制到剪贴板？\n\n" + data.data.key)
          ) {
            copyKey(data.data.key);
          }
        }, 300);
      }
    } else {
      throw new Error("Create failed");
    }
  } catch (e) {
    showToast("创建失败", "error");
  }
}

async function batchCreate() {
  const apiKey = await ensureApiKey();
  if (!apiKey) return;

  const count = parseInt(document.getElementById("batch-count").value) || 5;
  const prefix = document.getElementById("batch-prefix").value.trim();

  try {
    const res = await fetch("/api/v1/admin/keys/batch", {
      method: "POST",
      headers: {
        ...buildAuthHeaders(apiKey),
        "Content-Type": "application/json",
      },
      body: JSON.stringify({ count, prefix }),
    });

    if (res.ok) {
      showToast(`成功创建 ${count} 个 Key`, "success");
      closeBatchModal();
      loadKeys();
    } else {
      throw new Error("Batch create failed");
    }
  } catch (e) {
    showToast("批量创建失败", "error");
  }
}

async function updateKeyName() {
  const apiKey = await ensureApiKey();
  if (!apiKey) return;

  const id = document.getElementById("edit-key-id").value;
  const name = document.getElementById("edit-name").value.trim();

  try {
    const res = await fetch(`/api/v1/admin/keys/${id}`, {
      method: "PATCH",
      headers: {
        ...buildAuthHeaders(apiKey),
        "Content-Type": "application/json",
      },
      body: JSON.stringify({ name }),
    });

    if (res.ok) {
      showToast("更新成功", "success");
      closeEditModal();
      loadKeys();
    } else {
      throw new Error("Update failed");
    }
  } catch (e) {
    showToast("更新失败", "error");
  }
}

async function toggleKey(id, enabled) {
  const apiKey = await ensureApiKey();
  if (!apiKey) return;

  try {
    const res = await fetch(`/api/v1/admin/keys/${id}`, {
      method: "PATCH",
      headers: {
        ...buildAuthHeaders(apiKey),
        "Content-Type": "application/json",
      },
      body: JSON.stringify({ enabled }),
    });

    if (res.ok) {
      showToast(enabled ? "已启用" : "已禁用", "success");
      loadKeys();
    } else {
      throw new Error("Toggle failed");
    }
  } catch (e) {
    showToast("操作失败", "error");
  }
}

async function deleteKey(id) {
  if (!confirm("确定要删除这个 Key 吗？")) return;

  const apiKey = await ensureApiKey();
  if (!apiKey) return;

  try {
    const res = await fetch(`/api/v1/admin/keys/${id}`, {
      method: "DELETE",
      headers: buildAuthHeaders(apiKey),
    });

    if (res.ok) {
      showToast("删除成功", "success");
      selectedKeys.delete(id);
      loadKeys();
    } else {
      throw new Error("Delete failed");
    }
  } catch (e) {
    showToast("删除失败", "error");
  }
}

async function batchToggle(enabled) {
  const apiKey = await ensureApiKey();
  if (!apiKey) return;

  const ids = Array.from(selectedKeys);
  if (!ids.length) return;

  try {
    await Promise.all(
      ids.map((id) =>
        fetch(`/api/v1/admin/keys/${id}`, {
          method: "PATCH",
          headers: {
            ...buildAuthHeaders(apiKey),
            "Content-Type": "application/json",
          },
          body: JSON.stringify({ enabled }),
        }),
      ),
    );

    showToast(`已${enabled ? "启用" : "禁用"} ${ids.length} 个 Key`, "success");
    loadKeys();
  } catch (e) {
    showToast("操作失败", "error");
  }
}

async function batchDelete() {
  const ids = Array.from(selectedKeys);
  if (!ids.length) return;
  if (!confirm(`确定要删除选中的 ${ids.length} 个 Key 吗？`)) return;

  const apiKey = await ensureApiKey();
  if (!apiKey) return;

  try {
    await Promise.all(
      ids.map((id) =>
        fetch(`/api/v1/admin/keys/${id}`, {
          method: "DELETE",
          headers: buildAuthHeaders(apiKey),
        }),
      ),
    );

    showToast(`已删除 ${ids.length} 个 Key`, "success");
    selectedKeys.clear();
    loadKeys();
  } catch (e) {
    showToast("删除失败", "error");
  }
}

function resetKeysState() {
  keysData = [];
  selectedKeys = new Set();
  _domCache.clear();
  tableClickHandler = null;
}

function cleanupKeysPage() {
  resetKeysState();
  keysInitialized = false;
}

function initKeysPage() {
  cleanupKeysPage();
  keysInitialized = true;
  setupTableEventDelegation();
  loadKeys();
}

const keyActions = {
  loadKeys,
  openBatchModal,
  openAddModal,
  toggleSelectAll,
  batchToggle,
  batchDelete,
  createKey,
  batchCreate,
  updateKeyName,
  closeAddModal,
  closeBatchModal,
  closeEditModal,
};

function registerKeysPage() {
  window.GrokAdminPages = window.GrokAdminPages || {};
  window.GrokAdminPages.keys = {
    init: initKeysPage,
    cleanup: cleanupKeysPage,
    actions: keyActions,
  };
}

registerKeysPage();

if (!IS_SPA) {
  document.addEventListener("DOMContentLoaded", () => {
    keysInitialized = true;
    setupTableEventDelegation();
    loadKeys();
  });
}
})();
