let _headerLoaded = false;
let _headerLoadPromise = null;

function updateAdminNav(pathname) {
  const container = document.getElementById("app-header");
  if (!container) return;
  const links = container.querySelectorAll("a[data-nav]");
  links.forEach((link) => {
    const target = link.getAttribute("data-nav") || "";
    link.classList.toggle("active", target && pathname.startsWith(target));
  });
}

async function loadAdminHeader() {
  if (_headerLoadPromise) return _headerLoadPromise;
  _headerLoadPromise = (async () => {
    const container = document.getElementById("app-header");
    if (!container) return false;
    try {
      const res = await fetch("/static/common/header.html?v=3");
      if (!res.ok) return false;
      container.innerHTML = await res.text();
      updateAdminNav(window.location.pathname);
      if (typeof updateStorageModeButton === "function") {
        updateStorageModeButton();
      }
      _headerLoaded = true;
      window.dispatchEvent(new CustomEvent("admin:header-loaded"));
      return true;
    } catch (e) {
      // Fail silently to avoid breaking page load
      return false;
    }
  })();
  return _headerLoadPromise;
}

window.AdminHeader = {
  load: loadAdminHeader,
  updateActive: updateAdminNav,
  isLoaded: () => _headerLoaded,
};

if (document.readyState === "loading") {
  document.addEventListener("DOMContentLoaded", loadAdminHeader);
} else {
  loadAdminHeader();
}
