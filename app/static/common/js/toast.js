function showToast(message, type = 'success') {
  // Ensure container exists
  let container = document.getElementById('toast-container');
  if (!container) {
    container = document.createElement('div');
    container.id = 'toast-container';
    container.className = 'toast-container';
    document.body.appendChild(container);
  }

  const toast = document.createElement('div');
  const isSuccess = type === 'success';
  const iconClass = isSuccess ? 'text-green-600' : 'text-red-600';

  const iconSvg = isSuccess
    ? `<svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="3" stroke-linecap="round" stroke-linejoin="round"><polyline points="20 6 9 17 4 12"></polyline></svg>`
    : `<svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="3" stroke-linecap="round" stroke-linejoin="round"><line x1="18" y1="6" x2="6" y2="18"></line><line x1="6" y1="6" x2="18" y2="18"></line></svg>`;

  toast.className = `toast ${isSuccess ? 'toast-success' : 'toast-error'}`;

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

  container.appendChild(toast);

  // Remove after 3 seconds
  setTimeout(() => {
    toast.classList.add('out');
    toast.addEventListener('animationend', () => {
      if (toast.parentElement) {
        toast.parentElement.removeChild(toast);
      }
    });
  }, 3000);
}

(function showRateLimitNoticeOnce() {
  const noticeKey = 'grok2api_rate_limits_notice_v1';
  const noticeText = 'GROK官方服务 rate-limits 更新后暂时无法准确计算 Token 剩余，等待官方接口优化后持续修复';
  const path = window.location.pathname || '';

  if (!path.startsWith('/admin') || path.startsWith('/admin/login')) {
    return;
  }

  try {
    if (localStorage.getItem(noticeKey)) {
      return;
    }
    localStorage.setItem(noticeKey, '1');
  } catch (e) {
    // If storage is blocked, just skip the one-time guard.
  }

  const show = () => {
    if (typeof showToast === 'function') {
      showToast(noticeText, 'error');
    }
  };

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', show);
  } else {
    show();
  }
})();
