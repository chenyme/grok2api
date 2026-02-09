const APP_KEY_STORAGE = 'grok2api_app_key_session';
let cachedApiKey = null;

async function getStoredAppKey() {
  return sessionStorage.getItem(APP_KEY_STORAGE) || '';
}

async function storeAppKey(appKey) {
  const value = (appKey || '').trim();
  if (!value) {
    clearStoredAppKey();
    return;
  }
  sessionStorage.setItem(APP_KEY_STORAGE, value);
  cachedApiKey = null;
}

function clearStoredAppKey() {
  sessionStorage.removeItem(APP_KEY_STORAGE);
  cachedApiKey = null;
}

async function requestApiKey(appKey) {
  const value = (appKey || '').trim();
  if (!value) {
    throw new Error('Unauthorized');
  }

  const res = await fetch('/api/v1/admin/login', {
    method: 'POST',
    headers: { Authorization: `Bearer ${value}` },
  });

  if (!res.ok) {
    throw new Error('Unauthorized');
  }

  const data = await res.json().catch(() => ({}));
  if (!data || data.status !== 'success') {
    throw new Error('Unauthorized');
  }

  cachedApiKey = `Bearer ${value}`;
  return cachedApiKey;
}

async function ensureApiKey() {
  if (cachedApiKey) {
    return cachedApiKey;
  }

  const appKey = await getStoredAppKey();
  if (!appKey) {
    window.location.href = '/admin';
    return null;
  }

  try {
    return await requestApiKey(appKey);
  } catch (e) {
    clearStoredAppKey();
    window.location.href = '/admin';
    return null;
  }
}

function buildAuthHeaders(apiKey) {
  return apiKey ? { Authorization: apiKey } : {};
}

function logout() {
  clearStoredAppKey();
  window.location.href = '/admin';
}

async function fetchStorageType() {
  const apiKey = await ensureApiKey();
  if (apiKey === null) return null;
  try {
    const res = await fetch('/api/v1/admin/storage', {
      headers: buildAuthHeaders(apiKey)
    });
    if (!res.ok) return null;
    const data = await res.json();
    return (data && data.type) ? String(data.type) : null;
  } catch (e) {
    return null;
  }
}

function formatStorageLabel(type) {
  if (!type) return '-';
  const normalized = type.toLowerCase();
  const map = {
    local: 'local',
    mysql: 'mysql',
    pgsql: 'pgsql',
    postgres: 'pgsql',
    postgresql: 'pgsql',
    redis: 'redis'
  };
  return map[normalized] || '-';
}

async function updateStorageModeButton() {
  const btn = document.getElementById('storage-mode-btn');
  if (!btn) return;
  btn.textContent = '...';
  btn.title = '存储模式';
  btn.classList.remove('storage-ready');
  const storageType = await fetchStorageType();
  const label = formatStorageLabel(storageType);
  btn.textContent = label === '-' ? label : label.toUpperCase();
  btn.title = '存储模式';
  if (label !== '-') {
    btn.classList.add('storage-ready');
  }
}

if (document.readyState === 'loading') {
  document.addEventListener('DOMContentLoaded', updateStorageModeButton);
} else {
  updateStorageModeButton();
}
