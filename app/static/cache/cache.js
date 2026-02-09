(() => {
const IS_SPA = window.__GROK_ADMIN_SPA__ === true;

let apiKey = "";
let cacheInitialized = false;
let cacheKeydownHandler = null;
let cacheGridClickHandler = null;

// 缓存预览状态
const cachePreviewState = {
  type: "image", // 'image' or 'video'
  limit: 24,
  offset: 0,
  total: 0,
  items: [],
  loading: false,
};

// UI 元素缓存
const ui = {};
const byId = (id) => document.getElementById(id);

function cacheUI() {
  ui.imgCount = byId("img-count");
  ui.imgSize = byId("img-size");
  ui.videoCount = byId("video-count");
  ui.videoSize = byId("video-size");
  ui.onlineCount = byId("online-count");
  ui.onlineStatus = byId("online-status");
  ui.previewTypeLabel = byId("preview-type-label");
  ui.previewCount = byId("preview-count");
  ui.cacheGrid = byId("cache-grid");
  ui.loadMoreWrap = byId("load-more-wrap");
  ui.btnLoadMore = byId("btn-load-more");
  ui.btnTabImage = byId("btn-tab-image");
  ui.btnTabVideo = byId("btn-tab-video");
  ui.previewModal = byId("preview-modal");
  ui.previewMediaContainer = byId("preview-media-container");
  ui.previewFilename = byId("preview-filename");
  ui.previewMeta = byId("preview-meta");
  ui.confirmDialog = byId("confirm-dialog");
  ui.confirmMessage = byId("confirm-message");
  ui.confirmOk = byId("confirm-ok");
  ui.confirmCancel = byId("confirm-cancel");
}

function setText(el, text) {
  if (el) el.textContent = text;
}

// 初始化
async function init() {
  apiKey = await ensureApiKey();
  if (apiKey === null) return;
  cacheUI();
  setupConfirmDialog();
  setupCacheGridEventDelegation();
  await loadStats();
  await loadCachePreview(true);
}

// 性能优化：缓存网格事件委托
function setupCacheGridEventDelegation() {
  if (!ui.cacheGrid) return;
  if (cacheGridClickHandler) {
    ui.cacheGrid.removeEventListener("click", cacheGridClickHandler);
  }
  cacheGridClickHandler = (e) => {
    const card = e.target.closest(".cache-preview-item");
    if (!card) return;
    const index = parseInt(card.dataset.index, 10);
    if (isNaN(index)) return;
    const item = cachePreviewState.items[index];
    if (item) {
      openPreviewModal(item, cachePreviewState.type);
    }
  };
  ui.cacheGrid.addEventListener("click", cacheGridClickHandler);
}

// 设置确认对话框
let confirmResolver = null;

function setupConfirmDialog() {
  const dialog = ui.confirmDialog;
  if (!dialog) return;

  dialog.addEventListener("close", () => {
    if (!confirmResolver) return;
    const ok = dialog.returnValue === "ok";
    confirmResolver(ok);
    confirmResolver = null;
  });

  dialog.addEventListener("cancel", (event) => {
    event.preventDefault();
    dialog.close("cancel");
  });

  dialog.addEventListener("click", (event) => {
    if (event.target === dialog) {
      dialog.close("cancel");
    }
  });

  if (ui.confirmOk) {
    ui.confirmOk.addEventListener("click", () => dialog.close("ok"));
  }
  if (ui.confirmCancel) {
    ui.confirmCancel.addEventListener("click", () => dialog.close("cancel"));
  }
}

function confirmAction(message, options = {}) {
  const dialog = ui.confirmDialog;
  if (!dialog || typeof dialog.showModal !== "function") {
    return Promise.resolve(window.confirm(message));
  }
  if (ui.confirmMessage) ui.confirmMessage.textContent = message;
  if (ui.confirmOk) ui.confirmOk.textContent = options.okText || "确定";
  if (ui.confirmCancel)
    ui.confirmCancel.textContent = options.cancelText || "取消";
  return new Promise((resolve) => {
    confirmResolver = resolve;
    dialog.showModal();
  });
}

// 加载统计数据
async function loadStats() {
  if (!cacheInitialized) return null;
  try {
    const res = await fetch("/api/v1/admin/cache", {
      headers: buildAuthHeaders(apiKey),
    });

    if (res.status === 401) {
      logout();
      return;
    }

    const data = await res.json();
    applyStatsData(data);
    return data;
  } catch (e) {
    showToast("加载统计失败", "error");
    return null;
  }
}

function applyStatsData(data) {
  setText(ui.imgCount, data.local_image?.count || 0);
  setText(ui.imgSize, `${data.local_image?.size_mb || 0} MB`);
  setText(ui.videoCount, data.local_video?.count || 0);
  setText(ui.videoSize, `${data.local_video?.size_mb || 0} MB`);
  setText(ui.onlineCount, data.online?.count || 0);

  const status = data.online?.status || "not_loaded";
  const statusMap = {
    ok: { text: "连接正常", cls: "text-xs text-green-600 mt-1" },
    no_token: { text: "无可用 Token", cls: "text-xs text-orange-500 mt-1" },
    not_loaded: { text: "未加载", cls: "text-xs text-[var(--accents-4)] mt-1" },
  };
  const info = statusMap[status] || {
    text: "无法连接",
    cls: "text-xs text-red-500 mt-1",
  };
  if (ui.onlineStatus) {
    ui.onlineStatus.textContent = info.text;
    ui.onlineStatus.className = info.cls;
  }
}

// 切换缓存类型 (图片/视频)
function switchCacheType(type) {
  if (cachePreviewState.loading) return;
  cachePreviewState.type = type;
  updateTabButtons();
  loadCachePreview(true);
}

function updateTabButtons() {
  const { type } = cachePreviewState;
  if (ui.btnTabImage) {
    ui.btnTabImage.classList.toggle("active", type === "image");
  }
  if (ui.btnTabVideo) {
    ui.btnTabVideo.classList.toggle("active", type === "video");
  }
  if (ui.previewTypeLabel) {
    ui.previewTypeLabel.textContent = type === "image" ? "图片" : "视频";
  }
}

// 加载缓存预览
async function loadCachePreview(reset = false) {
  if (!cacheInitialized) return;
  if (cachePreviewState.loading) return;
  cachePreviewState.loading = true;

  if (reset) {
    cachePreviewState.offset = 0;
    cachePreviewState.items = [];
    if (ui.cacheGrid) {
      ui.cacheGrid.innerHTML = '<div class="cache-empty">加载中...</div>';
    }
  }

  try {
    const { type, limit, offset } = cachePreviewState;
    const params = new URLSearchParams({
      type,
      page: "1",
      page_size: String(limit + offset),
    });
    const res = await fetch(`/api/v1/admin/cache/list?${params.toString()}`, {
      headers: buildAuthHeaders(apiKey),
    });

    if (!res.ok) {
      throw new Error("加载失败");
    }

    const data = await res.json();
    const items = Array.isArray(data.items) ? data.items : [];
    cachePreviewState.total = data.total || items.length;
    cachePreviewState.items = items;
    cachePreviewState.offset = items.length;

    renderCachePreview();
  } catch (e) {
    if (ui.cacheGrid) {
      ui.cacheGrid.innerHTML = '<div class="cache-empty">加载失败</div>';
    }
    showToast("加载缓存列表失败", "error");
  } finally {
    cachePreviewState.loading = false;
  }
}

// 渲染缓存预览网格
function renderCachePreview() {
  if (!cacheInitialized) return;
  const { items, type, total, offset } = cachePreviewState;

  // 更新计数
  setText(ui.previewCount, total);

  // 显示/隐藏加载更多按钮
  if (ui.loadMoreWrap) {
    ui.loadMoreWrap.classList.toggle("hidden", offset >= total);
  }

  if (!ui.cacheGrid) return;

  if (!items || items.length === 0) {
    ui.cacheGrid.innerHTML = '<div class="cache-empty">暂无缓存文件</div>';
    return;
  }

  const fragment = document.createDocumentFragment();

  items.forEach((item, index) => {
    const card = document.createElement("div");
    card.className = "cache-preview-item";
    card.dataset.index = index;

    // 缩略图区域
    const thumbWrap = document.createElement("div");
    thumbWrap.className = "cache-thumb-wrap";

    if (type === "image") {
      const img = document.createElement("img");
      img.src =
        item.preview_url || `/v1/files/image/${encodeURIComponent(item.name)}`;
      img.alt = item.name;
      img.className = "cache-thumb-img";
      img.loading = "lazy";
      img.onerror = () => {
        img.style.display = "none";
        thumbWrap.innerHTML = '<div class="cache-thumb-placeholder">图片</div>';
      };
      thumbWrap.appendChild(img);
    } else {
      // 视频 - 使用 video 标签显示第一帧作为封面
      const video = document.createElement("video");
      video.src = `/v1/files/video/${encodeURIComponent(item.name)}`;
      video.className = "cache-thumb-video-preview";
      video.preload = "metadata";
      video.muted = true;
      video.playsInline = true;
      // 加载后跳到第一帧
      video.addEventListener("loadeddata", () => {
        video.currentTime = 0.1;
      });
      video.onerror = () => {
        video.style.display = "none";
        thumbWrap.innerHTML = `
          <div class="cache-thumb-video">
            <svg width="40" height="40" viewBox="0 0 24 24" fill="currentColor">
              <path d="M8 5v14l11-7z"/>
            </svg>
          </div>
        `;
      };
      thumbWrap.appendChild(video);
    }

    // 信息区域
    const infoWrap = document.createElement("div");
    infoWrap.className = "cache-item-info";

    const nameEl = document.createElement("div");
    nameEl.className = "cache-item-name";
    nameEl.textContent = item.name;
    nameEl.title = item.name;

    const metaEl = document.createElement("div");
    metaEl.className = "cache-item-meta";
    metaEl.textContent = `${formatSize(item.size_bytes)} • ${formatTime(item.mtime_ms)}`;

    const previewLink = document.createElement("div");
    previewLink.className = "cache-item-preview-link";
    previewLink.textContent = "预览";

    infoWrap.appendChild(nameEl);
    infoWrap.appendChild(metaEl);
    infoWrap.appendChild(previewLink);

    card.appendChild(thumbWrap);
    card.appendChild(infoWrap);
    fragment.appendChild(card);
  });

  ui.cacheGrid.innerHTML = "";
  ui.cacheGrid.appendChild(fragment);
}

// 格式化文件大小
function formatSize(bytes) {
  if (bytes === 0 || bytes === null || bytes === undefined) return "-";
  const kb = 1024;
  const mb = kb * 1024;
  if (bytes >= mb) return `${(bytes / mb).toFixed(1)} MB`;
  if (bytes >= kb) return `${(bytes / kb).toFixed(1)} KB`;
  return `${bytes} B`;
}

// 格式化时间
function formatTime(ms) {
  if (!ms) return "";
  const dt = new Date(ms);
  return dt.toLocaleString("zh-CN", { hour12: false });
}

// 加载更多
function loadMore() {
  if (cachePreviewState.loading) return;
  cachePreviewState.offset += cachePreviewState.limit;
  loadCachePreview(false);
}

// 刷新缓存
async function refreshCache() {
  if (!cacheInitialized) return;
  await loadStats();
  await loadCachePreview(true);
  showToast("刷新完成", "success");
}

// 打开预览模态框
function openPreviewModal(item, type) {
  if (!cacheInitialized) return;
  if (!ui.previewModal || !ui.previewMediaContainer) return;

  const url =
    type === "image"
      ? `/v1/files/image/${encodeURIComponent(item.name)}`
      : `/v1/files/video/${encodeURIComponent(item.name)}`;

  ui.previewMediaContainer.innerHTML = "";

  if (type === "image") {
    const img = document.createElement("img");
    img.src = url;
    img.alt = item.name;
    img.className = "preview-modal-image";
    ui.previewMediaContainer.appendChild(img);
  } else {
    const video = document.createElement("video");
    video.src = url;
    video.controls = true;
    video.autoplay = true;
    video.className = "preview-modal-video";
    ui.previewMediaContainer.appendChild(video);
  }

  if (ui.previewFilename) {
    ui.previewFilename.textContent = item.name;
  }
  if (ui.previewMeta) {
    ui.previewMeta.textContent = `${formatSize(item.size_bytes)} • ${formatTime(item.mtime_ms)}`;
  }

  ui.previewModal.classList.remove("hidden");
  document.body.style.overflow = "hidden";
}

// 关闭预览模态框
function closePreviewModal(event) {
  if (event && event.target !== ui.previewModal) return;
  if (!ui.previewModal) return;

  // 停止视频播放
  const video = ui.previewMediaContainer?.querySelector("video");
  if (video) {
    video.pause();
    video.src = "";
  }

  ui.previewModal.classList.add("hidden");
  document.body.style.overflow = "";
}

// 清空缓存
async function clearCache(type) {
  const label = type === "image" ? "图片" : "视频";
  const ok = await confirmAction(`确定要清空所有本地${label}缓存吗？`, {
    okText: "清空",
  });
  if (!ok) return;

  try {
    const res = await fetch("/api/v1/admin/cache/clear", {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        ...buildAuthHeaders(apiKey),
      },
      body: JSON.stringify({ type }),
    });

    const data = await res.json();
    if (data.status === "success") {
      showToast(`清理成功，释放 ${data.result.size_mb} MB`, "success");
      await loadStats();
      if (cachePreviewState.type === type) {
        await loadCachePreview(true);
      }
    } else {
      showToast("清理失败", "error");
    }
  } catch (e) {
    showToast("请求失败", "error");
  }
}

function resetCacheState() {
  apiKey = "";
  cachePreviewState.type = "image";
  cachePreviewState.limit = 24;
  cachePreviewState.offset = 0;
  cachePreviewState.total = 0;
  cachePreviewState.items = [];
  cachePreviewState.loading = false;

  Object.keys(ui).forEach((key) => delete ui[key]);
  confirmResolver = null;
  cacheGridClickHandler = null;
}

function cleanupCachePage() {
  if (cacheKeydownHandler) {
    document.removeEventListener("keydown", cacheKeydownHandler);
    cacheKeydownHandler = null;
  }
  if (ui.previewModal && !ui.previewModal.classList.contains("hidden")) {
    closePreviewModal();
  }
  document.body.style.overflow = "";
  resetCacheState();
  cacheInitialized = false;
}

function initCachePage() {
  cleanupCachePage();
  cacheInitialized = true;
  cacheKeydownHandler = (e) => {
    if (
      e.key === "Escape" &&
      ui.previewModal &&
      !ui.previewModal.classList.contains("hidden")
    ) {
      closePreviewModal();
    }
  };
  document.addEventListener("keydown", cacheKeydownHandler);
  init();
}

const cacheActions = {
  switchCacheType,
  refreshCache,
  clearCache,
  loadMore,
  closePreviewModal,
};

function registerCachePage() {
  window.GrokAdminPages = window.GrokAdminPages || {};
  window.GrokAdminPages.cache = {
    init: initCachePage,
    cleanup: cleanupCachePage,
    actions: cacheActions,
  };
}

registerCachePage();

if (!IS_SPA) {
  window.addEventListener("load", () => {
    cacheInitialized = true;
    init();
  });
}
})();
