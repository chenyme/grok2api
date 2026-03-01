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
  const noticeText = 'GROK 官方网页更新后未真实暴露 rate-limits 接口，导致无法准确计算 Token 剩余，请耐心等待官方接口上线，目前自动刷新后会更新为 8 次';
  const path = window.location.pathname || '';

  if (!path.startsWith('/admin') || path.startsWith('/admin/login')) {
    return;
  }

  try {
    if (localStorage.getItem(noticeKey)) {
      return;
    }
  } catch (e) {
    // If storage is blocked, keep showing dialog.
  }

  const show = () => {
    const backdrop = document.createElement('div');
    backdrop.className = 'notice-dialog-backdrop';

    const dialog = document.createElement('div');
    dialog.className = 'notice-dialog';
    dialog.setAttribute('role', 'dialog');
    dialog.setAttribute('aria-modal', 'true');

    const title = document.createElement('div');
    title.className = 'notice-dialog-title';
    title.textContent = '提示';

    const content = document.createElement('div');
    content.className = 'notice-dialog-content';
    content.textContent = noticeText;

    const actions = document.createElement('div');
    actions.className = 'notice-dialog-actions';

    const confirmBtn = document.createElement('button');
    confirmBtn.type = 'button';
    confirmBtn.className = 'notice-dialog-confirm';
    confirmBtn.textContent = '我知道了';

    actions.appendChild(confirmBtn);
    dialog.appendChild(title);
    dialog.appendChild(content);
    dialog.appendChild(actions);
    backdrop.appendChild(dialog);
    document.body.appendChild(backdrop);

    const prevOverflow = document.body.style.overflow;
    document.body.style.overflow = 'hidden';

    confirmBtn.addEventListener('click', () => {
      try {
        localStorage.setItem(noticeKey, '1');
      } catch (e) {
        // ignore
      }
      document.body.style.overflow = prevOverflow;
      backdrop.remove();
    });
  };

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', show);
  } else {
    show();
  }
})();
