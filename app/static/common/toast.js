// 性能优化：缓存容器引用
let _toastContainer = null;

function showToast(message, type = "success") {
  // 性能优化：使用缓存的容器引用
  if (!_toastContainer || !_toastContainer.isConnected) {
    _toastContainer = document.getElementById("toast-container");
    if (!_toastContainer) {
      _toastContainer = document.createElement("div");
      _toastContainer.id = "toast-container";
      _toastContainer.className = "toast-container";
      document.body.appendChild(_toastContainer);
    }
  }

  const toast = document.createElement("div");
  const isSuccess = type === "success";

  const iconSvg = isSuccess
    ? `<svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="3" stroke-linecap="round" stroke-linejoin="round"><polyline points="20 6 9 17 4 12"></polyline></svg>`
    : `<svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="3" stroke-linecap="round" stroke-linejoin="round"><line x1="18" y1="6" x2="6" y2="18"></line><line x1="6" y1="6" x2="18" y2="18"></line></svg>`;

  toast.className = `toast ${isSuccess ? "toast-success" : "toast-error"}`;

  // Basic HTML escaping for message
  const escapedMessage = message
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#039;");

  toast.innerHTML = `
        <div class="toast-icon">
          ${iconSvg}
        </div>
        <div class="toast-content">${escapedMessage}</div>
      `;

  _toastContainer.appendChild(toast);

  // 性能优化：使用 { once: true } 自动移除事件监听器
  setTimeout(() => {
    toast.classList.add("out");
    toast.addEventListener(
      "animationend",
      () => {
        toast.remove();
      },
      { once: true },
    );
  }, 3000);
}
