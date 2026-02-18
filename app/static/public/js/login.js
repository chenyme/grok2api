const publicKeyInput = document.getElementById('public-key-input');
if (publicKeyInput) {
  publicKeyInput.addEventListener('keypress', (e) => {
    if (e.key === 'Enter') login();
  });
}

async function requestPublicLogin(key) {
  const headers = key ? { 'Authorization': `Bearer ${key}` } : {};
  const res = await fetch('/v1/public/verify', {
    method: 'GET',
    headers
  });
  return res.ok;
}

async function login() {
  const input = (publicKeyInput ? publicKeyInput.value : '').trim();
  try {
    const ok = await requestPublicLogin(input);
    if (ok) {
      await storePublicKey(input);
      window.location.href = '/imagine';
    } else {
      showToast('密钥无效', 'error');
    }
  } catch (e) {
    showToast('连接失败', 'error');
  }
}

(async () => {
  try {
    const stored = await getStoredPublicKey();
    if (stored) {
      const ok = await requestPublicLogin(stored);
      if (ok) {
        window.location.href = '/imagine';
        return;
      }
      clearStoredPublicKey();
    }

    const ok = await requestPublicLogin('');
    if (ok) {
      window.location.href = '/imagine';
    }
  } catch (e) {
    return;
  }
})();
