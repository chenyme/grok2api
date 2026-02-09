const usernameInput = document.getElementById("username-input");
const passwordInput = document.getElementById("password-input");
const loginForm = document.getElementById("login-form");
const loginButton = loginForm?.querySelector("button[type='submit']") || null;

let isLoggingIn = false;

// Enter key: username -> password, password -> submit
if (usernameInput) {
  usernameInput.addEventListener("keypress", (e) => {
    if (e.key === "Enter") {
      e.preventDefault();
      passwordInput?.focus();
    }
  });
}

if (passwordInput) {
  passwordInput.addEventListener("keypress", (e) => {
    if (e.key === "Enter") {
      e.preventDefault();
      login();
    }
  });
}

// Form submit handler
if (loginForm) {
  loginForm.addEventListener("submit", (e) => {
    e.preventDefault();
    login();
  });
}

async function requestLogin(username, password) {
  const res = await fetch("/api/v1/admin/login", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ username, password }),
  });
  if (!res.ok) return null;
  return await res.json();
}

async function login() {
  if (isLoggingIn) return;

  const username = (usernameInput ? usernameInput.value : "").trim();
  const password = (passwordInput ? passwordInput.value : "").trim();

  if (!username) {
    showToast("请输入账户", "error");
    usernameInput?.focus();
    return;
  }

  if (!password) {
    showToast("请输入密码", "error");
    passwordInput?.focus();
    return;
  }

  isLoggingIn = true;
  if (loginButton) loginButton.disabled = true;

  try {
    const result = await requestLogin(username, password);
    if (result && result.status === "success") {
      // Store password for subsequent API calls
      await storeAppKey(password);
      window.location.href = "/admin/token";
    } else {
      showToast("账户或密码错误", "error");
      passwordInput?.select();
    }
  } catch (e) {
    showToast("连接失败", "error");
  } finally {
    isLoggingIn = false;
    if (loginButton) loginButton.disabled = false;
  }
}

// Auto-redirect if already logged in (check with stored key)
(async () => {
  const existingKey = await getStoredAppKey();
  if (!existingKey) return;
  // Try to validate with a simple request
  try {
    const res = await fetch("/api/v1/admin/config", {
      headers: { Authorization: `Bearer ${existingKey}` },
    });
    if (res.ok) window.location.href = "/admin/token";
  } catch (e) {
    // ignore
  }
})();
