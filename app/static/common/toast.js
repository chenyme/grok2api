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
