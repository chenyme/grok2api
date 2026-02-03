const apiKeyInput = document.getElementById('api-key-input');
if (apiKeyInput) {
  apiKeyInput.addEventListener('keypress', (e) => {
    if (e.key === 'Enter') login();
  });
}

async function requestLogin(key) {
  const res = await fetch('/api/v1/admin/login', {
    method: 'POST',
    headers: { 'Authorization': `Bearer ${key}` }
  });
  return res.ok;
}

async function login() {
  const input = (apiKeyInput ? apiKeyInput.value : '').trim();
  if (!input) return;

  try {
    const ok = await requestLogin(input);
    if (ok) {
      await storeAppKey(input);
      window.location.href = '/admin/token';
    } else {
      showToast('密钥无效', 'error');
    }
  } catch (e) {
    showToast('连接失败', 'error');
  }
}

// Auto-redirect checks
(async () => {
  const existingKey = await getStoredAppKey();
  if (!existingKey) return;
  try {
    const ok = await requestLogin(existingKey);
    if (ok) window.location.href = '/admin/token';
  } catch (e) {
    return;
  }
})();
