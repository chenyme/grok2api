const APP_KEY_STORAGE = 'grok2api_app_key';
const PUBLIC_KEY_STORAGE = 'grok2api_public_key';
const APP_KEY_ENC_PREFIX = 'enc:v1:';
const APP_KEY_XOR_PREFIX = 'enc:xor:';
const APP_KEY_SECRET = 'grok2api-admin-key';
let cachedAdminKey = null;
let cachedPublicKey = null;

const textEncoder = new TextEncoder();
const textDecoder = new TextDecoder();

function toBase64(bytes) {
  let binary = '';
  bytes.forEach(b => { binary += String.fromCharCode(b); });
  return btoa(binary);
}

function fromBase64(base64) {
  const binary = atob(base64);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) {
    bytes[i] = binary.charCodeAt(i);
  }
  return bytes;
}

function xorCipher(bytes, keyBytes) {
  const out = new Uint8Array(bytes.length);
  for (let i = 0; i < bytes.length; i++) {
    out[i] = bytes[i] ^ keyBytes[i % keyBytes.length];
  }
  return out;
}

function xorEncrypt(plain) {
  const data = textEncoder.encode(plain);
  const key = textEncoder.encode(APP_KEY_SECRET);
  const cipher = xorCipher(data, key);
  return `${APP_KEY_XOR_PREFIX}${toBase64(cipher)}`;
}

function xorDecrypt(stored) {
  if (!stored.startsWith(APP_KEY_XOR_PREFIX)) return stored;
  const payload = stored.slice(APP_KEY_XOR_PREFIX.length);
  const data = fromBase64(payload);
  const key = textEncoder.encode(APP_KEY_SECRET);
  const plain = xorCipher(data, key);
  return textDecoder.decode(plain);
}

async function deriveKey(salt) {
  const keyMaterial = await crypto.subtle.importKey(
    'raw',
    textEncoder.encode(APP_KEY_SECRET),
    'PBKDF2',
    false,
    ['deriveKey']
  );
  return crypto.subtle.deriveKey(
    {
      name: 'PBKDF2',
      salt,
      iterations: 100000,
      hash: 'SHA-256'
    },
    keyMaterial,
    { name: 'AES-GCM', length: 256 },
    false,
    ['encrypt', 'decrypt']
  );
}

async function encryptAppKey(plain) {
  if (!plain) return '';
  if (!crypto?.subtle) return xorEncrypt(plain);
  const salt = crypto.getRandomValues(new Uint8Array(16));
  const iv = crypto.getRandomValues(new Uint8Array(12));
  const key = await deriveKey(salt);
  const cipher = await crypto.subtle.encrypt(
    { name: 'AES-GCM', iv },
    key,
    textEncoder.encode(plain)
  );
  return `${APP_KEY_ENC_PREFIX}${toBase64(salt)}:${toBase64(iv)}:${toBase64(new Uint8Array(cipher))}`;
}

async function decryptAppKey(stored) {
  if (!stored) return '';
  if (stored.startsWith(APP_KEY_XOR_PREFIX)) return xorDecrypt(stored);
  if (!stored.startsWith(APP_KEY_ENC_PREFIX)) return stored;
  if (!crypto?.subtle) return '';
  const parts = stored.split(':');
  if (parts.length !== 5) return '';
  const salt = fromBase64(parts[2]);
  const iv = fromBase64(parts[3]);
  const cipher = fromBase64(parts[4]);
  const key = await deriveKey(salt);
  const plain = await crypto.subtle.decrypt(
    { name: 'AES-GCM', iv },
    key,
    cipher
  );
  return textDecoder.decode(plain);
}

async function getStoredAppKey() {
  const stored = localStorage.getItem(APP_KEY_STORAGE) || '';
  if (!stored) return '';
  try {
    return await decryptAppKey(stored);
  } catch (e) {
    clearStoredAppKey();
    return '';
  }
}

async function getStoredPublicKey() {
  const stored = localStorage.getItem(PUBLIC_KEY_STORAGE) || '';
  if (!stored) return '';
  try {
    return await decryptAppKey(stored);
  } catch (e) {
    clearStoredPublicKey();
    return '';
  }
}

async function storeAppKey(appKey) {
  if (!appKey) {
    clearStoredAppKey();
    return;
  }
  const encrypted = await encryptAppKey(appKey);
  localStorage.setItem(APP_KEY_STORAGE, encrypted || '');
}

async function storePublicKey(publicKey) {
  if (!publicKey) {
    clearStoredPublicKey();
    return;
  }
  const encrypted = await encryptAppKey(publicKey);
  localStorage.setItem(PUBLIC_KEY_STORAGE, encrypted || '');
}

function clearStoredAppKey() {
  localStorage.removeItem(APP_KEY_STORAGE);
  cachedAdminKey = null;
}

function clearStoredPublicKey() {
  localStorage.removeItem(PUBLIC_KEY_STORAGE);
  cachedPublicKey = null;
}

async function verifyKey(url, key) {
  const headers = key ? { 'Authorization': `Bearer ${key}` } : {};
  const res = await fetch(url, { method: 'GET', headers });
  return res.ok;
}

async function ensureAdminKey() {
  if (cachedAdminKey) return cachedAdminKey;
  const appKey = await getStoredAppKey();
  if (!appKey) {
    window.location.href = '/admin/login';
    return null;
  }
  try {
    const ok = await verifyKey('/v1/admin/verify', appKey);
    if (!ok) throw new Error('Unauthorized');
    cachedAdminKey = `Bearer ${appKey}`;
    return cachedAdminKey;
  } catch (e) {
    clearStoredAppKey();
    window.location.href = '/admin/login';
    return null;
  }
}

async function ensurePublicKey() {
  if (cachedPublicKey !== null) return cachedPublicKey;

  const key = await getStoredPublicKey();
  if (!key) {
    try {
      const ok = await verifyKey('/v1/public/verify', '');
      if (ok) {
        cachedPublicKey = '';
        return cachedPublicKey;
      }
    } catch (e) {
      // ignore
    }
    return null;
  }

  if (!key) {
    return null;
  }

  try {
    const ok = await verifyKey('/v1/public/verify', key);
    if (!ok) throw new Error('Unauthorized');
    cachedPublicKey = `Bearer ${key}`;
    return cachedPublicKey;
  } catch (e) {
    clearStoredPublicKey();
    return null;
  }
}

function buildAuthHeaders(apiKey) {
  return apiKey ? { 'Authorization': apiKey } : {};
}

function logout() {
  clearStoredAppKey();
  clearStoredPublicKey();
  window.location.href = '/admin/login';
}

function publicLogout() {
  clearStoredPublicKey();
  window.location.href = '/login';
}

async function fetchStorageType() {
  const apiKey = await ensureAdminKey();
  if (apiKey === null) return null;
  try {
    const res = await fetch('/v1/admin/storage', {
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
