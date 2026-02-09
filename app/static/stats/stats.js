(() => {
  const IS_SPA = window.__GROK_ADMIN_SPA__ === true;

  let hourlyChart = null;
  let dailyChart = null;
  let modelChart = null;
  let statsInitialized = false;

  // 当前 tab
  let currentTab = "statistics";

  // 日志分页状态
  const logPagination = {
    page: 1,
    pageSize: 20,
    total: 0,
    logs: [],
  };

  // 性能优化：DOM 元素缓存
  const _domCache = new Map();
  function getCachedEl(id) {
    if (!_domCache.has(id)) {
      _domCache.set(id, document.getElementById(id));
    }
    return _domCache.get(id);
  }

  // 性能优化：Tab 元素缓存
  let _tabItems = null;
  let _panels = null;
  function getTabItems() {
    if (!_tabItems) _tabItems = document.querySelectorAll(".tab-item");
    return _tabItems;
  }
  function getPanels() {
    if (!_panels) _panels = document.querySelectorAll(".panel");
    return _panels;
  }

  // 性能优化：安全销毁 Chart（防止内存泄漏）
  function destroyChart(chart) {
    if (chart) {
      chart.destroy();
      chart = null;
    }
    return null;
  }

  async function loadStats() {
    if (!statsInitialized) return;
    const apiKey = await ensureApiKey();
    if (!apiKey) return;

    try {
      const res = await fetch("/api/v1/admin/stats/requests?hours=24&days=7", {
        headers: buildAuthHeaders(apiKey),
      });

      if (!res.ok) {
        if (res.status === 400) {
          showToast("统计功能未启用", "warning");
          return;
        }
        throw new Error("Failed to load stats");
      }

      const data = await res.json();
      if (data.status === "success" && data.data) {
        renderStats(data.data);
      }
    } catch (e) {
      showToast("加载统计数据失败", "error");
    }
  }

  function renderStats(data) {
    if (!statsInitialized) return;
    // 汇总数据 - 使用缓存的 DOM 元素
    const summary = data.summary || {};
    const statTotal = getCachedEl("stat-total");
    const statSuccess = getCachedEl("stat-success");
    const statFailed = getCachedEl("stat-failed");
    const statRate = getCachedEl("stat-rate");

    if (statTotal) statTotal.textContent = summary.total || 0;
    if (statSuccess) statSuccess.textContent = summary.success || 0;
    if (statFailed) statFailed.textContent = summary.failed || 0;
    if (statRate) statRate.textContent = (summary.success_rate || 0) + "%";

    // 小时图表
    const hourly = data.hourly || [];
    renderHourlyChart(hourly);

    // 天图表
    const daily = data.daily || [];
    renderDailyChart(daily);

    // 模型图表
    const models = data.models || [];
    renderModelChart(models);
  }

  function renderHourlyChart(hourly) {
    const canvas = getCachedEl("hourlyChart");
    if (!canvas) return;
    const ctx = canvas.getContext("2d");
    const labels = hourly.map((h) => h.hour);
    const successData = hourly.map((h) => h.success);
    const failedData = hourly.map((h) => h.failed);

    hourlyChart = destroyChart(hourlyChart);

    hourlyChart = new Chart(ctx, {
      type: "line",
      data: {
        labels,
        datasets: [
          {
            label: "成功",
            data: successData,
            borderColor: "#10b981",
            backgroundColor: "rgba(16, 185, 129, 0.1)",
            fill: true,
            tension: 0.3,
            pointRadius: 0,
          },
          {
            label: "失败",
            data: failedData,
            borderColor: "#ef4444",
            backgroundColor: "rgba(239, 68, 68, 0.1)",
            fill: true,
            tension: 0.3,
            pointRadius: 0,
          },
        ],
      },
      options: {
        responsive: true,
        maintainAspectRatio: false,
        plugins: {
          legend: { display: true, position: "top" },
        },
        scales: {
          x: { grid: { display: false } },
          y: { beginAtZero: true, grid: { color: "#f0f0f0" } },
        },
      },
    });
  }

  function renderDailyChart(daily) {
    const canvas = getCachedEl("dailyChart");
    if (!canvas) return;
    const ctx = canvas.getContext("2d");
    const labels = daily.map((d) => d.date);
    const totalData = daily.map((d) => d.total);
    const successData = daily.map((d) => d.success);

    dailyChart = destroyChart(dailyChart);

    dailyChart = new Chart(ctx, {
      type: "bar",
      data: {
        labels,
        datasets: [
          {
            label: "总请求",
            data: totalData,
            backgroundColor: "rgba(59, 130, 246, 0.8)",
            borderRadius: 4,
          },
          {
            label: "成功",
            data: successData,
            backgroundColor: "rgba(16, 185, 129, 0.8)",
            borderRadius: 4,
          },
        ],
      },
      options: {
        responsive: true,
        maintainAspectRatio: false,
        plugins: {
          legend: { display: true, position: "top" },
        },
        scales: {
          x: { grid: { display: false } },
          y: { beginAtZero: true, grid: { color: "#f0f0f0" } },
        },
      },
    });
  }

  function renderModelChart(models) {
    const canvas = getCachedEl("modelChart");
    if (!canvas) return;
    const ctx = canvas.getContext("2d");
    const labels = models.map((m) => m.model);
    const counts = models.map((m) => m.count);

    modelChart = destroyChart(modelChart);

    const colors = [
      "#3b82f6",
      "#10b981",
      "#f59e0b",
      "#ef4444",
      "#8b5cf6",
      "#ec4899",
      "#06b6d4",
      "#84cc16",
      "#f97316",
      "#6366f1",
    ];

    modelChart = new Chart(ctx, {
      type: "bar",
      data: {
        labels,
        datasets: [
          {
            label: "调用次数",
            data: counts,
            backgroundColor: colors.slice(0, labels.length),
            borderRadius: 4,
          },
        ],
      },
      options: {
        indexAxis: "y",
        responsive: true,
        maintainAspectRatio: false,
        plugins: {
          legend: { display: false },
        },
        scales: {
          x: { beginAtZero: true, grid: { color: "#f0f0f0" } },
          y: { grid: { display: false } },
        },
      },
    });
  }

  async function resetStats() {
    if (!confirm("确定要重置所有统计数据吗？此操作不可撤销。")) return;

    const apiKey = await ensureApiKey();
    if (!apiKey) return;

    try {
      const res = await fetch("/api/v1/admin/stats/reset", {
        method: "POST",
        headers: buildAuthHeaders(apiKey),
      });

      if (res.ok) {
        showToast("统计数据已重置", "success");
        loadStats();
      } else {
        throw new Error("Reset failed");
      }
    } catch (e) {
      showToast("重置失败", "error");
    }
  }

  async function loadLogs() {
    if (!statsInitialized) return;
    const apiKey = await ensureApiKey();
    if (!apiKey) return;

    try {
      const res = await fetch("/api/v1/admin/logs?limit=500", {
        headers: buildAuthHeaders(apiKey),
      });

      if (!res.ok) {
        if (res.status === 400) {
          showToast("日志功能未启用", "warning");
          return;
        }
        throw new Error("Failed to load logs");
      }

      const data = await res.json();
      if (data.status === "success") {
        logPagination.logs = data.data || [];
        logPagination.total = logPagination.logs.length;
        logPagination.page = 1;
        renderLogs();
      }
    } catch (e) {
      showToast("加载日志失败", "error");
    }
  }

  function renderLogs() {
    if (!statsInitialized) return;
    const { logs, page, pageSize, total } = logPagination;
    const start = (page - 1) * pageSize;
    const end = Math.min(start + pageSize, total);
    const pageLogs = logs.slice(start, end);

    document.getElementById("log-count").textContent = total;

    const tbody = document.getElementById("log-table-body");
    if (!logs.length) {
      tbody.innerHTML =
        '<tr><td colspan="7" class="table-empty">暂无日志记录</td></tr>';
      renderPagination();
      return;
    }

    tbody.innerHTML = pageLogs
      .map((log, idx) => {
        const safeTime = escapeHtml(log.time || "-");
        const safeIp = escapeHtml(log.ip || "-");
        const safeModel = escapeHtml(log.model || "-");
        const safeDuration = escapeHtml(
          log.duration ? `${log.duration}s` : "-",
        );
        const statusCode = Number(log.status);
        const statusClass =
          statusCode === 200
            ? "success"
            : statusCode === 499
              ? "warn"
              : "error";
        const safeStatus = escapeHtml(
          log.status == null ? "-" : String(log.status),
        );
        const safeKeyName = escapeHtml(log.key_name || "-");
        const errorText = getLogErrorText(log);
        const errorLink = errorText
          ? `<span class="error-link" onclick="GrokAdminPages.stats.actions.showErrorDetail(${start + idx})" title="点击查看详情">错误详情</span>`
          : "-";

        return `
    <tr>
      <td class="font-mono text-xs">${safeTime}</td>
      <td class="font-mono text-xs">${safeIp}</td>
      <td>${safeModel}</td>
      <td>${safeDuration}</td>
      <td>
        <span class="log-status ${statusClass}">
          ${safeStatus}
        </span>
      </td>
      <td class="font-mono text-xs">${safeKeyName}</td>
      <td class="log-error">${errorLink}</td>
    </tr>
  `;
      })
      .join("");

    // 存储日志以供详情查看
    window._currentLogs = logs;

    renderPagination();
  }

  function renderPagination() {
    if (!statsInitialized) return;
    const { page, pageSize, total } = logPagination;
    const totalPages = Math.ceil(total / pageSize);

    let paginationEl = document.getElementById("log-pagination");
    if (!paginationEl) {
      const wrapper = document.querySelector("#panel-logs .log-table-wrapper");
      if (!wrapper) return;
      paginationEl = document.createElement("div");
      paginationEl.id = "log-pagination";
      paginationEl.className = "log-pagination";
      wrapper.after(paginationEl);
    }

    if (totalPages <= 1) {
      paginationEl.innerHTML = "";
      return;
    }

    const start = (page - 1) * pageSize + 1;
    const end = Math.min(page * pageSize, total);

    paginationEl.innerHTML = `
    <div class="pagination-info">
      显示 ${start}-${end} 条，共 ${total} 条
    </div>
    <div class="pagination-buttons">
      <button onclick="GrokAdminPages.stats.actions.goToPage(1)" ${page === 1 ? "disabled" : ""} class="pagination-btn">首页</button>
      <button onclick="GrokAdminPages.stats.actions.goToPage(${page - 1})" ${page === 1 ? "disabled" : ""} class="pagination-btn">上一页</button>
      <span class="pagination-current">第 ${page} / ${totalPages} 页</span>
      <button onclick="GrokAdminPages.stats.actions.goToPage(${page + 1})" ${page === totalPages ? "disabled" : ""} class="pagination-btn">下一页</button>
      <button onclick="GrokAdminPages.stats.actions.goToPage(${totalPages})" ${page === totalPages ? "disabled" : ""} class="pagination-btn">末页</button>
    </div>
  `;
  }

  function goToPage(newPage) {
    if (!statsInitialized) return;
    const totalPages = Math.ceil(logPagination.total / logPagination.pageSize);
    if (newPage < 1 || newPage > totalPages) return;
    logPagination.page = newPage;
    renderLogs();
  }

  function showErrorDetail(idx) {
    if (!statsInitialized) return;
    const logs = window._currentLogs || [];
    const log = logs[idx];
    if (!log) return;

    const errorText = getLogErrorText(log);
    if (!errorText) return;

    // 创建弹窗显示错误详情
    const modal = document.createElement("div");
    modal.className = "error-modal-overlay";
    modal.innerHTML = `
    <div class="error-modal">
      <div class="error-modal-header">
        <span class="error-modal-title">错误详情</span>
        <button class="error-modal-close" onclick="this.closest('.error-modal-overlay').remove()">&times;</button>
      </div>
      <div class="error-modal-body">
        <div class="error-meta">
          <span><b>时间:</b> ${escapeHtml(log.time || "-")}</span>
          <span><b>模型:</b> ${escapeHtml(log.model || "-")}</span>
          <span><b>状态:</b> ${escapeHtml(log.status == null ? "-" : String(log.status))}</span>
        </div>
        <pre class="error-content">${escapeHtml(errorText)}</pre>
      </div>
    </div>
  `;
    document.body.appendChild(modal);

    // 点击背景关闭
    modal.addEventListener("click", (e) => {
      if (e.target === modal) modal.remove();
    });
  }

  function getLogErrorText(log) {
    if (!log || typeof log !== "object") {
      return "";
    }

    const explicitError = typeof log.error === "string" ? log.error.trim() : "";
    if (explicitError) {
      return explicitError;
    }

    const statusCode = Number(log.status);
    if (!Number.isFinite(statusCode) || statusCode === 200) {
      return "";
    }

    if (statusCode === 499) {
      return "客户端提前断开连接（手动停止、刷新页面或网络中断）。";
    }

    return `请求失败（HTTP ${statusCode}），未返回详细错误信息。`;
  }

  function escapeHtml(text) {
    const div = document.createElement("div");
    div.textContent = text || "";
    return div.innerHTML;
  }

  async function clearLogs() {
    if (!confirm("确定要清空所有日志吗？此操作不可撤销。")) return;

    const apiKey = await ensureApiKey();
    if (!apiKey) return;

    try {
      const res = await fetch("/api/v1/admin/logs/clear", {
        method: "POST",
        headers: buildAuthHeaders(apiKey),
      });

      if (res.ok) {
        showToast("日志已清空", "success");
        loadLogs();
      } else {
        throw new Error("Clear failed");
      }
    } catch (e) {
      showToast("清空失败", "error");
    }
  }

  // 刷新当前 tab
  function refreshCurrentTab() {
    if (currentTab === "statistics") {
      loadStats();
    } else if (currentTab === "logs") {
      loadLogs();
    }
    showToast("刷新成功", "success");
  }

  function switchTab(tab) {
    currentTab = tab;

    // 性能优化：使用缓存的 Tab 元素
    getTabItems().forEach((btn) => {
      btn.classList.toggle("active", btn.dataset.tab === tab);
    });

    // 性能优化：使用缓存的 Panel 元素
    getPanels().forEach((panel) => {
      panel.classList.add("hidden");
    });
    const targetPanel = getCachedEl("panel-" + tab);
    if (targetPanel) targetPanel.classList.remove("hidden");

    // 加载对应数据
    if (tab === "statistics") {
      loadStats();
    } else if (tab === "logs") {
      loadLogs();
    }
  }

  function resetStatsState() {
    hourlyChart = destroyChart(hourlyChart);
    dailyChart = destroyChart(dailyChart);
    modelChart = destroyChart(modelChart);

    currentTab = "statistics";
    logPagination.page = 1;
    logPagination.pageSize = 20;
    logPagination.total = 0;
    logPagination.logs = [];

    _domCache.clear();
    _tabItems = null;
    _panels = null;

    window._currentLogs = [];
    document
      .querySelectorAll(".error-modal-overlay")
      .forEach((el) => el.remove());
  }

  function cleanupStatsPage() {
    resetStatsState();
    statsInitialized = false;
  }

  function initStatsPage() {
    cleanupStatsPage();
    statsInitialized = true;
    loadStats();
  }

  const statsActions = {
    switchTab,
    refreshCurrentTab,
    resetStats,
    clearLogs,
    goToPage,
    showErrorDetail,
  };

  function registerStatsPage() {
    window.GrokAdminPages = window.GrokAdminPages || {};
    window.GrokAdminPages.stats = {
      init: initStatsPage,
      cleanup: cleanupStatsPage,
      actions: statsActions,
    };
  }

  registerStatsPage();

  if (!IS_SPA) {
    document.addEventListener("DOMContentLoaded", () => {
      statsInitialized = true;
      loadStats();
    });
  }
})();
