window.renderSiteFooter = async function renderSiteFooter() {
  if (document.body?.classList.contains('webui-embedded-page')) return;
  if (document.querySelector('.site-footer')) return;

  let version = '';
  try {
    const res = await fetch('/meta', { cache: 'no-store' });
    if (res.ok) {
      const data = await res.json();
      version = String(data?.version || '').trim();
    }
  } catch {}

  const footer = document.createElement('div');
  footer.className = 'site-footer';
  footer.setAttribute('aria-hidden', 'true');

  const link = (text, href) => {
    const node = document.createElement('a');
    node.href = href;
    node.target = '_blank';
    node.rel = 'noopener';
    node.textContent = text;
    return node;
  };

  const sep = () => {
    const node = document.createElement('span');
    node.textContent = '·';
    return node;
  };

  const brand = link('Grok2API', 'https://github.com/chenyme/grok2api');
  footer.appendChild(brand);

  footer.appendChild(sep());

  const author = link('@Chenyme', 'https://blog.cheny.me/');
  footer.appendChild(author);

  if (version) {
    footer.appendChild(sep());

    const ver = link(`v${version}`, 'https://github.com/chenyme/grok2api/releases');
    ver.className = 'site-footer-version';
    footer.appendChild(ver);
  }

  document.body.appendChild(footer);
};

const _bootSiteFooter = () => {
  if (typeof window.renderSiteFooter === 'function') {
    void window.renderSiteFooter();
  }
};

if (document.readyState === 'loading') {
  document.addEventListener('DOMContentLoaded', _bootSiteFooter, { once: true });
} else {
  _bootSiteFooter();
}

window.addEventListener('pageshow', _bootSiteFooter);
