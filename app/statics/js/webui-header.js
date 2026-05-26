window.renderWebuiHeader = async function renderWebuiHeader() {
  const mount = document.getElementById('webui-header');
  if (!mount || mount.children.length) return;
  const scriptVersion = (() => {
    try {
      const script = document.querySelector('script[src*="/static/js/webui-header.js"]');
      if (!script) return 'v1';
      return new URL(script.src, window.location.href).searchParams.get('v') || 'v1';
    } catch {
      return 'v1';
    }
  })();
  const HEADER_HTML_CACHE_KEY = `grok2api.webui_header_html.${scriptVersion}`;
  const META_VERSION_CACHE_KEY = `grok2api.meta_version.${scriptVersion}`;
  let appVersion = '';

  const readSessionCache = (key) => {
    try {
      return sessionStorage.getItem(key) || '';
    } catch {
      return '';
    }
  };

  const writeSessionCache = (key, value) => {
    if (!value) return;
    try {
      sessionStorage.setItem(key, value);
    } catch {}
  };

  const languageCodes = {
    zh: 'CN',
    en: 'EN',
    ja: 'JA',
    es: 'ES',
    de: 'DE',
    fr: 'FR',
  };

  const initLanguageMenu = () => {
    const menu = mount.querySelector('#hd-lang-menu');
    const trigger = mount.querySelector('#hd-lang-trigger');
    const code = mount.querySelector('#hd-lang-code');
    const options = Array.from(mount.querySelectorAll('.admin-lang-option'));
    if (!menu || !trigger || !code || !options.length) return;

    const close = () => {
      menu.classList.remove('open');
      trigger.setAttribute('aria-expanded', 'false');
    };

    const sync = () => {
      const current = window.I18n?.getLang?.() || localStorage.getItem('grok2api_lang') || 'zh';
      code.textContent = languageCodes[current] || current.toUpperCase();
      options.forEach((option) => {
        option.classList.toggle('active', option.dataset.lang === current);
      });
    };

    trigger.addEventListener('click', (event) => {
      event.stopPropagation();
      const open = !menu.classList.contains('open');
      menu.classList.toggle('open', open);
      trigger.setAttribute('aria-expanded', open ? 'true' : 'false');
    });

    options.forEach((option) => {
      option.addEventListener('click', () => {
        const lang = option.dataset.lang;
        if (!lang) return;
        close();
        if (window.I18n?.setLang) {
          I18n.setLang(lang);
        } else {
          localStorage.setItem('grok2api_lang', lang);
          location.reload();
        }
      });
    });

    document.addEventListener('click', (event) => {
      const target = event.target;
      if (!(target instanceof Node) || !menu.contains(target)) close();
    });
    document.addEventListener('keydown', (event) => {
      if (event.key === 'Escape') close();
    });

    sync();
    return sync;
  };

  const applyHeaderI18n = () => {
    if (window.I18n?.apply) I18n.apply(mount);
    const trigger = mount.querySelector('#hd-lang-trigger');
    if (trigger) {
      const label = window.t ? t('header.languageLabel') : 'Language';
      trigger.title = label;
      trigger.setAttribute('aria-label', label);
    }
    const logout = mount.querySelector('#hd-logout');
    if (logout) {
      const hidden = mount.dataset.hideLogout === 'true';
      logout.hidden = hidden;
      if (!hidden) {
        const label = window.t ? t('header.logout') : 'Logout';
        logout.title = label;
        logout.setAttribute('aria-label', label);
      }
    }
  };

  const loadVersion = async () => {
    const cachedVersion = window.__grok2apiMetaVersion || readSessionCache(META_VERSION_CACHE_KEY);
    if (cachedVersion) {
      appVersion = String(cachedVersion).trim();
      window.__grok2apiMetaVersion = appVersion;
      return;
    }
    try {
      const res = await fetch('/meta');
      if (!res.ok) throw new Error('meta unavailable');
      const data = await res.json();
      appVersion = String(data?.version || '').trim();
      window.__grok2apiMetaVersion = appVersion;
      writeSessionCache(META_VERSION_CACHE_KEY, appVersion);
    } catch {
      appVersion = '';
    }
  };

  const applyVersion = () => {
    const right = mount.querySelector('.admin-header-right');
    if (!right) return;
    let node = mount.querySelector('#hd-version');
    if (!appVersion) {
      node?.remove();
      return;
    }
    if (!node) {
      node = document.createElement('span');
      node.id = 'hd-version';
      node.className = 'admin-header-version';
      right.insertBefore(node, right.firstChild);
    }
    const value = `v${appVersion}`;
    node.textContent = value;
    node.title = value;
  };

  await loadVersion();

  try {
    const cachedHtml = window.__grok2apiWebuiHeaderHtml || readSessionCache(HEADER_HTML_CACHE_KEY);
    if (cachedHtml) {
      mount.innerHTML = cachedHtml;
    } else {
      const res = await fetch('/static/webui/header.html');
      if (!res.ok) throw new Error('header unavailable');
      const html = await res.text();
      mount.innerHTML = html;
      window.__grok2apiWebuiHeaderHtml = html;
      writeSessionCache(HEADER_HTML_CACHE_KEY, html);
    }
  } catch {
    mount.innerHTML = `
      <header class="admin-header webui-header-bar">
        <div class="admin-header-inner webui-header-inner">
          <div class="admin-brand-wrap">
            <a href="https://github.com/chenyme/grok2api" target="_blank" rel="noopener" class="admin-brand-link">
              <span class="admin-brand">Grok2API</span>
            </a>
            <a href="https://blog.cheny.me/" target="_blank" rel="noopener" class="admin-username" id="hd-user">@Chenyme</a>
          </div>
          <nav class="admin-nav">
            <a href="/webui/chat" class="admin-nav-link" data-nav="/webui/chat" data-i18n="webui.header.chat">聊天</a>
            <a href="/webui/images" class="admin-nav-link" data-nav="/webui/images" data-i18n="webui.header.images">生图</a>
            <a href="/webui/masonry" class="admin-nav-link" data-nav="/webui/masonry" data-i18n="webui.header.masonry">Masonry</a>
            <a href="/webui/chatkit" class="admin-nav-link" data-nav="/webui/chatkit" data-i18n="webui.header.chatkit">ChatKit</a>
          </nav>
          <div class="admin-header-right">
            <div class="admin-lang-menu" id="hd-lang-menu">
              <button type="button" class="btn admin-header-control admin-lang-trigger" id="hd-lang-trigger" aria-label="Language" aria-haspopup="menu" aria-expanded="false">
                <span class="admin-lang-trigger-code" id="hd-lang-code">CN</span>
                <svg viewBox="0 0 24 24" aria-hidden="true">
                  <path d="m7 10 5 5 5-5"/>
                </svg>
              </button>
              <div class="admin-lang-popover" id="hd-lang-popover" role="menu" aria-labelledby="hd-lang-trigger">
                <button type="button" class="admin-lang-option" data-lang="zh" role="menuitem">
                  <span class="admin-lang-option-code">CN</span>
                  <span class="admin-lang-option-name">简体中文</span>
                </button>
                <button type="button" class="admin-lang-option" data-lang="en" role="menuitem">
                  <span class="admin-lang-option-code">EN</span>
                  <span class="admin-lang-option-name">English</span>
                </button>
                <button type="button" class="admin-lang-option" data-lang="ja" role="menuitem">
                  <span class="admin-lang-option-code">JA</span>
                  <span class="admin-lang-option-name">日本語</span>
                </button>
                <button type="button" class="admin-lang-option" data-lang="es" role="menuitem">
                  <span class="admin-lang-option-code">ES</span>
                  <span class="admin-lang-option-name">Español</span>
                </button>
                <button type="button" class="admin-lang-option" data-lang="de" role="menuitem">
                  <span class="admin-lang-option-code">DE</span>
                  <span class="admin-lang-option-name">Deutsch</span>
                </button>
                <button type="button" class="admin-lang-option" data-lang="fr" role="menuitem">
                  <span class="admin-lang-option-code">FR</span>
                  <span class="admin-lang-option-name">Français</span>
                </button>
              </div>
            </div>
            <button onclick="webuiLogout()" class="btn admin-header-control admin-header-icon-btn" id="hd-logout" aria-label="Logout" title="Logout">
              <svg viewBox="0 0 24 24" aria-hidden="true">
                <path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4"/>
                <path d="M16 17l5-5-5-5"/>
                <path d="M21 12H9"/>
              </svg>
            </button>
          </div>
        </div>
      </header>`;
  }

  const active = mount.dataset.active || location.pathname;
  mount.querySelectorAll('[data-nav]').forEach((link) => {
    link.classList.toggle('active', link.dataset.nav === active);
  });

  const syncLanguageMenu = initLanguageMenu();
  applyHeaderI18n();
  applyVersion();
  syncLanguageMenu?.();
};
