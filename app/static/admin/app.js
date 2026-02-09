(() => {
  const ROUTES = [
    {
      path: "/admin/token",
      key: "token",
      title: "Grok2API - Token",
      template: "/static/token/token.html",
      style: "/static/token/token.css?v=11",
    },
    {
      path: "/admin/imagine",
      key: "imagine",
      title: "Grok2API - Imagine",
      template: "/static/imagine/imagine.html",
      style: "/static/imagine/imagine.css?v=3",
    },
    {
      path: "/admin/voice",
      key: "voice",
      title: "Grok2API - Voice",
      template: "/static/voice/voice.html",
      style: "/static/voice/voice.css?v=15",
    },
    {
      path: "/admin/keys",
      key: "keys",
      title: "Grok2API - Keys",
      template: "/static/keys/keys.html",
      style: "/static/keys/keys.css?v=1",
    },
    {
      path: "/admin/stats",
      key: "stats",
      title: "Grok2API - Stats",
      template: "/static/stats/stats.html",
      style: "/static/stats/stats.css?v=5",
    },
    {
      path: "/admin/cache",
      key: "cache",
      title: "Grok2API - Cache",
      template: "/static/cache/cache.html",
      style: "/static/cache/cache.css?v=11",
    },
    {
      path: "/admin/config",
      key: "config",
      title: "Grok2API - Config",
      template: "/static/config/config.html",
      style: "/static/config/config.css?v=10",
    },
  ];

  const ROUTE_MAP = new Map(ROUTES.map((route) => [route.path, route]));
  const DEFAULT_ROUTE = "/admin/token";
  const templateCache = new Map();

  let currentRouteKey = null;
  let navToken = 0;

  function normalizePath(path) {
    if (!path) return DEFAULT_ROUTE;
    const queryIndex = path.indexOf("?");
    const hashIndex = path.indexOf("#");
    let clean = path;
    if (queryIndex >= 0) clean = clean.slice(0, queryIndex);
    if (hashIndex >= 0) clean = clean.slice(0, hashIndex);
    if (clean.length > 1 && clean.endsWith("/")) {
      clean = clean.slice(0, -1);
    }
    return clean || DEFAULT_ROUTE;
  }

  function resolveRoute(path) {
    const normalized = normalizePath(path);
    if (ROUTE_MAP.has(normalized)) {
      return { route: ROUTE_MAP.get(normalized), redirected: false };
    }
    return { route: ROUTE_MAP.get(DEFAULT_ROUTE), redirected: true };
  }

  async function ensureHeaderLoaded() {
    if (window.AdminHeader && typeof window.AdminHeader.load === "function") {
      await window.AdminHeader.load();
      return;
    }

    const headerEl = document.getElementById("app-header");
    if (headerEl && headerEl.querySelector("a[data-nav]")) return;

    await new Promise((resolve) => {
      const onLoaded = () => resolve();
      window.addEventListener("admin:header-loaded", onLoaded, { once: true });
      setTimeout(resolve, 1500);
    });
  }

  function updateActiveNav(path) {
    if (window.AdminHeader && typeof window.AdminHeader.updateActive === "function") {
      window.AdminHeader.updateActive(path);
      return;
    }
    const container = document.getElementById("app-header");
    if (!container) return;
    const links = container.querySelectorAll("a[data-nav]");
    links.forEach((link) => {
      const target = link.getAttribute("data-nav") || "";
      link.classList.toggle("active", target && path.startsWith(target));
    });
  }

  function setPageStyle(href) {
    const link = document.getElementById("page-style");
    if (!link || !href) return;
    if (link.getAttribute("href") === href) return;
    link.setAttribute("href", href);
  }

  function setPageTitle(title) {
    if (title) document.title = title;
  }

  async function fetchTemplate(url) {
    if (templateCache.has(url)) return templateCache.get(url);
    const res = await fetch(url, { cache: "no-store" });
    if (!res.ok) throw new Error(`Failed to load ${url}`);
    const text = await res.text();
    templateCache.set(url, text);
    return text;
  }

  function parseTemplate(html) {
    const doc = new DOMParser().parseFromString(html, "text/html");
    const main = doc.querySelector("main");
    const overlays = [];

    if (doc.body) {
      Array.from(doc.body.children).forEach((child) => {
        if (child.tagName === "SCRIPT") return;
        if (child.tagName === "MAIN") return;
        const id = child.id || "";
        if (id === "app-header" || id === "app-footer" || id === "toast-container") {
          return;
        }
        overlays.push(child);
      });
    }

    return { main, overlays };
  }

  function mountMain(main) {
    const host = document.getElementById("app-main");
    if (!host) return;

    if (!main) {
      host.className = "space-y-6 flex-1 container mx-auto max-w-7xl px-8 py-8";
      host.textContent = "Failed to load page.";
      return;
    }

    host.className = main.className || "";
    const style = main.getAttribute("style");
    if (style) {
      host.setAttribute("style", style);
    } else {
      host.removeAttribute("style");
    }

    const nodes = Array.from(main.childNodes);
    host.replaceChildren(...nodes);
  }

  function mountOverlays(nodes) {
    const host = document.getElementById("app-overlays");
    if (!host) return;
    host.replaceChildren();
    if (!nodes || !nodes.length) return;
    nodes.forEach((node) => host.appendChild(node));
  }

  function runCleanup(key) {
    const registry = window.GrokAdminPages || {};
    const page = key ? registry[key] : null;
    if (page && typeof page.cleanup === "function") {
      page.cleanup();
    }
  }

  function runInit(key) {
    const registry = window.GrokAdminPages || {};
    const page = key ? registry[key] : null;
    if (page && typeof page.init === "function") {
      page.init();
    }
  }

  async function loadRoute(route, options = {}) {
    const token = ++navToken;

    runCleanup(currentRouteKey);

    try {
      await ensureHeaderLoaded();
      if (token !== navToken) return;

      const templateHtml = await fetchTemplate(route.template);
      if (token !== navToken) return;

      const { main, overlays } = parseTemplate(templateHtml);
      if (token !== navToken) return;

      setPageStyle(route.style);
      mountMain(main);
      mountOverlays(overlays);
      updateActiveNav(route.path);
      setPageTitle(route.title);

      currentRouteKey = route.key;
      runInit(route.key);
      window.scrollTo(0, 0);
    } catch (err) {
      const host = document.getElementById("app-main");
      if (host) {
        host.className = "space-y-6 flex-1 container mx-auto max-w-7xl px-8 py-8";
        host.textContent = "Failed to load page.";
      }
      console.error(err);
    }
  }

  async function navigate(path, options = {}) {
    const { route, redirected } = resolveRoute(path);
    if (!route) return;

    const isSame = currentRouteKey === route.key;
    if (!options.force && isSame) {
      updateActiveNav(route.path);
      if (!options.fromPop && normalizePath(window.location.pathname) !== route.path) {
        history.pushState({}, "", route.path);
      }
      return;
    }

    await loadRoute(route, options);

    if (!options.fromPop) {
      const method = options.replace || redirected ? "replaceState" : "pushState";
      history[method]({}, "", route.path);
    }
  }

  function handleLinkClick(event) {
    if (event.defaultPrevented || event.button !== 0) return;
    if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;

    const link = event.target.closest("a");
    if (!link) return;
    if (link.target && link.target !== "_self") return;

    const href = link.getAttribute("href") || "";
    if (!href.startsWith("/admin/")) return;
    const normalized = normalizePath(href);
    if (!ROUTE_MAP.has(normalized)) return;

    event.preventDefault();
    navigate(normalized);
  }

  function startRouter() {
    document.addEventListener("click", handleLinkClick);
    window.addEventListener("popstate", () => {
      navigate(window.location.pathname, { fromPop: true, replace: true });
    });

    navigate(window.location.pathname, { replace: true });
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", startRouter);
  } else {
    startRouter();
  }
})();
