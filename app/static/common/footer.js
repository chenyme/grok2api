async function loadAdminFooter() {
  const container = document.getElementById('app-footer');
  if (!container) return;
  try {
    const res = await fetch('/static/common/footer.html?v=1');
    if (!res.ok) return;
    container.innerHTML = await res.text();
  } catch (e) {
    // Fail silently to avoid breaking page load
  }
}

if (document.readyState === 'loading') {
  document.addEventListener('DOMContentLoaded', loadAdminFooter);
} else {
  loadAdminFooter();
}
