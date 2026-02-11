/**
 * 使用记录页面逻辑
 */

// ==================== State ====================
let apiKey = null;
let currentPage = 1;
let pageSize = 20;
let totalItems = 0;
let isLoading = false;

// ==================== Utils ====================

function authHeaders(extra) {
  return { ...buildAuthHeaders(apiKey), ...(extra || {}) };
}

function formatTime(ts) {
  if (!ts) return "-";
  const d = new Date(ts * 1000);
  const pad = (n) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

function formatDuration(ms) {
  if (!ms && ms !== 0) return "-";
  if (ms < 1000) return `${ms}ms`;
  return `${(ms / 1000).toFixed(1)}s`;
}

function maskToken(hash) {
  if (!hash) return "-";
  if (hash.length > 20) return hash.slice(0, 8) + "..." + hash.slice(-8);
  return hash;
}

function getTimeRange(value) {
  if (!value) return { start: null, end: null };
  const now = Math.floor(Date.now() / 1000);
  const map = {
    "1h": 3600,
    "24h": 86400,
    "7d": 86400 * 7,
    "30d": 86400 * 30,
  };
  const seconds = map[value];
  if (!seconds) return { start: null, end: null };
  return { start: now - seconds, end: null };
}

// ==================== API ====================

async function fetchStats() {
  try {
    const resp = await fetch("/api/v1/admin/logs/stats", {
      headers: authHeaders(),
    });
    if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
    const data = await resp.json();

    document.getElementById("stat-total").textContent =
      data.total != null ? data.total : "-";
    document.getElementById("stat-success").textContent =
      data.success != null ? data.success : "-";
    document.getElementById("stat-error").textContent =
      data.error != null ? data.error : "-";
    document.getElementById("stat-today").textContent =
      data.today != null ? data.today : "-";
  } catch (e) {
    console.error("Failed to fetch stats:", e);
  }
}

async function fetchLogs() {
  if (isLoading) return;
  isLoading = true;

  const loading = document.getElementById("loading");
  const empty = document.getElementById("empty-state");
  const tbody = document.getElementById("logs-table-body");

  loading.classList.remove("hidden");
  empty.classList.add("hidden");
  tbody.innerHTML = "";

  try {
    const params = new URLSearchParams();
    params.set("page", currentPage);
    params.set("page_size", pageSize);

    const type = document.getElementById("filter-type").value;
    const model = document.getElementById("filter-model").value.trim();
    const status = document.getElementById("filter-status").value;
    const timeVal = document.getElementById("filter-time").value;

    if (type) params.set("type", type);
    if (model) params.set("model", model);
    if (status) params.set("status", status);

    const { start, end } = getTimeRange(timeVal);
    if (start) params.set("start_time", start);
    if (end) params.set("end_time", end);

    const resp = await fetch(`/api/v1/admin/logs?${params}`, {
      headers: authHeaders(),
    });
    if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
    const data = await resp.json();

    totalItems = data.total || 0;
    const logs = data.data || [];

    loading.classList.add("hidden");

    if (logs.length === 0) {
      empty.classList.remove("hidden");
    } else {
      renderTable(logs);
    }

    updatePagination();
  } catch (e) {
    console.error("Failed to fetch logs:", e);
    loading.textContent = "加载失败";
    if (typeof showToast === "function") showToast("加载失败: " + e.message, "error");
  } finally {
    isLoading = false;
  }
}

// ==================== Render ====================

function typeBadge(type) {
  const map = {
    chat: ["Chat", "type-chat"],
    image: ["Image", "type-image"],
    video: ["Video", "type-video"],
  };
  const [label, cls] = map[type] || [type || "-", "badge-gray"];
  return `<span class="badge ${cls}">${label}</span>`;
}

function statusBadge(status) {
  if (status === "success") return `<span class="badge badge-green">成功</span>`;
  if (status === "error") return `<span class="badge badge-red">失败</span>`;
  return `<span class="badge badge-gray">${status || "-"}</span>`;
}

function renderTable(logs) {
  const tbody = document.getElementById("logs-table-body");
  const rows = [];

  for (const log of logs) {
    const errorHtml =
      log.status === "error" && log.error_message
        ? `<div class="error-cell">${statusBadge(log.status)}<div class="error-tip">${escapeHtml(log.error_message)}</div></div>`
        : statusBadge(log.status);

    rows.push(`<tr>
      <td class="text-left font-mono text-xs">${formatTime(log.created_at)}</td>
      <td>${typeBadge(log.type)}</td>
      <td class="text-left font-mono text-xs">${escapeHtml(log.model || "-")}</td>
      <td>${errorHtml}</td>
      <td>${log.is_stream ? '<span class="badge badge-blue">是</span>' : '<span class="badge badge-gray">否</span>'}</td>
      <td class="font-mono text-xs">${formatDuration(log.use_time)}</td>
      <td class="text-left font-mono text-xs">${escapeHtml(maskToken(log.token_hash))}</td>
      <td class="text-left font-mono text-xs">${escapeHtml(log.ip || "-")}</td>
    </tr>`);
  }

  tbody.innerHTML = rows.join("");
}

function escapeHtml(str) {
  if (!str) return "";
  return str
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;");
}

// ==================== Pagination ====================

function updatePagination() {
  const totalPages = Math.max(1, Math.ceil(totalItems / pageSize));
  document.getElementById("pagination-info").textContent =
    `第 ${currentPage} / ${totalPages} 页 · 共 ${totalItems} 条`;
  document.getElementById("page-prev").disabled = currentPage <= 1;
  document.getElementById("page-next").disabled = currentPage >= totalPages;
}

function goPrevPage() {
  if (currentPage > 1) {
    currentPage--;
    fetchLogs();
  }
}

function goNextPage() {
  const totalPages = Math.ceil(totalItems / pageSize);
  if (currentPage < totalPages) {
    currentPage++;
    fetchLogs();
  }
}

function changePageSize() {
  pageSize = parseInt(document.getElementById("page-size").value, 10) || 20;
  currentPage = 1;
  fetchLogs();
}

function applyFilters() {
  currentPage = 1;
  fetchLogs();
}

// ==================== Actions ====================

function refreshData() {
  fetchStats();
  applyFilters();
}

function openCleanupModal() {
  const modal = document.getElementById("cleanup-modal");
  modal.classList.remove("hidden");
  requestAnimationFrame(() => modal.classList.add("is-open"));
}

function closeCleanupModal() {
  const modal = document.getElementById("cleanup-modal");
  modal.classList.remove("is-open");
  setTimeout(() => modal.classList.add("hidden"), 200);
}

async function submitCleanup() {
  const range = document.getElementById("cleanup-range").value;
  const now = Math.floor(Date.now() / 1000);
  let before;

  if (range === "all") {
    before = now;
  } else {
    const map = { "7d": 86400 * 7, "30d": 86400 * 30, "90d": 86400 * 90 };
    before = now - (map[range] || 86400 * 30);
  }

  try {
    const resp = await fetch("/api/v1/admin/logs", {
      method: "DELETE",
      headers: authHeaders({ "Content-Type": "application/json" }),
      body: JSON.stringify({ before_timestamp: before }),
    });
    if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
    const data = await resp.json();
    closeCleanupModal();
    if (typeof showToast === "function")
      showToast(`已清理 ${data.deleted || 0} 条记录`, "success");
    refreshData();
  } catch (e) {
    if (typeof showToast === "function") showToast("清理失败: " + e.message, "error");
  }
}

// ==================== Init ====================

async function init() {
  apiKey = await ensureApiKey();
  if (!apiKey) return;
  fetchStats();
  fetchLogs();
}

document.addEventListener("DOMContentLoaded", init);
