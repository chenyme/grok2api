document.getElementById('api-key-input').addEventListener('keypress', function (e) {
  if (e.key === 'Enter') login();
});

async function login() {
  const input = document.getElementById('api-key-input').value.trim();
  if (!input) return;

  try {
    const res = await fetch('/api/v1/admin/login', {
      method: 'POST',
      headers: { 'Authorization': `Bearer ${input}` }
    });

    if (res.ok) {
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
  if (existingKey) {
    fetch('/api/v1/admin/login', {
      method: 'POST',
      headers: { 'Authorization': `Bearer ${existingKey}` }
    }).then(res => {
      if (res.ok) window.location.href = '/admin/token';
    });
    return;
  }
})();
