(() => {
  const tabs = Array.from(document.querySelectorAll('.admin-experiments-tab'));
  const frame = document.getElementById('experimentFrame');
  const routes = {
    chat: '/admin/chat?embed=1',
    images: '/admin/images-tools?embed=1',
    masonry: '/admin/masonry?embed=1',
    chatkit: '/admin/chatkit?embed=1',
  };
  const aliases = {
    webchat: 'chat',
    image: 'images',
    video: 'images',
    'image-edit': 'images',
    edit: 'images',
    text2image: 'images',
    text2video: 'images',
    image2video: 'images',
  };

  const normalizeTab = (value) => {
    const raw = String(value || '').replace(/^#/, '').trim().toLowerCase();
    return routes[raw] ? raw : (aliases[raw] || 'chat');
  };

  const readTabFromLocation = () => {
    const params = new URLSearchParams(window.location.search);
    return normalizeTab(window.location.hash || params.get('tab') || params.get('pane'));
  };

  const setActiveTab = (tab, { updateHash = true } = {}) => {
    const next = normalizeTab(tab);
    tabs.forEach((item) => {
      const active = item.dataset.tab === next;
      item.classList.toggle('is-active', active);
      item.setAttribute('aria-selected', active ? 'true' : 'false');
    });
    if (frame && frame.getAttribute('src') !== routes[next]) {
      frame.setAttribute('src', routes[next]);
    }
    if (updateHash && window.location.hash !== `#${next}`) {
      history.replaceState(null, '', `${window.location.pathname}${window.location.search}#${next}`);
    }
  };

  const ensureAccess = async () => {
    const key = await adminKey.get();
    if (!key || !await verifyKey(`${ADMIN_API}/verify`, key).catch(() => false)) {
      location.href = '/admin/login';
      return false;
    }
    return true;
  };

  const boot = async () => {
    await renderAdminHeader?.();
    await renderSiteFooter?.();
    if (!await ensureAccess()) return;
    tabs.forEach((tab) => {
      tab.addEventListener('click', () => setActiveTab(tab.dataset.tab));
    });
    setActiveTab(readTabFromLocation(), { updateHash: false });
  };

  window.addEventListener('hashchange', () => setActiveTab(readTabFromLocation(), { updateHash: false }));
  boot().catch(() => {
    if (typeof showToast === 'function') showToast('实验生成页面初始化失败', 'error');
  });
})();
