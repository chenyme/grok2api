(() => {
  const params = new URLSearchParams(window.location.search);
  const modeButtons = Array.from(document.querySelectorAll('.admin-image-mode-tab'));
  const modePanels = Array.from(document.querySelectorAll('.admin-image-mode-panel'));
  const modelSelect = document.getElementById('modelSelect');
  const sizeSelect = document.getElementById('sizeSelect');
  const countInput = document.getElementById('countInput');
  const promptInput = document.getElementById('promptInput');

  const aliases = {
    image: 'text-image',
    images: 'text-image',
    text2image: 'text-image',
    edit: 'image-edit',
    image2image: 'image-edit',
    video: 'text-video',
    text2video: 'text-video',
    image2video: 'image-video',
  };

  const normalizeMode = (value) => {
    const raw = String(value || '').replace(/^#/, '').trim().toLowerCase();
    return modePanels.some((panel) => panel.dataset.modePanel === raw) ? raw : (aliases[raw] || 'text-image');
  };

  const syncControls = (mode) => {
    if (!modelSelect) return;
    if (mode === 'text-image') {
      modelSelect.value = 'grok-imagine-image-lite';
      if (countInput) countInput.max = '4';
      return;
    }
    if (mode === 'image-edit') {
      modelSelect.value = 'grok-imagine-image-edit';
      if (countInput) countInput.max = '2';
      return;
    }
    if (mode === 'text-video' || mode === 'image-video') {
      modelSelect.value = 'grok-imagine-video';
      if (countInput) countInput.value = '1';
      if (countInput) countInput.max = '1';
      if (sizeSelect) sizeSelect.value = '720x1280';
    }
  };

  const loadActiveFrame = (mode) => {
    const panel = modePanels.find((item) => item.dataset.modePanel === mode);
    const frame = panel?.querySelector('iframe[data-src]');
    if (frame && !frame.src) frame.src = frame.dataset.src;
  };

  const setMode = (mode, { updateUrl = true } = {}) => {
    const next = normalizeMode(mode);
    modeButtons.forEach((button) => {
      const active = button.dataset.mode === next;
      button.classList.toggle('is-active', active);
      button.setAttribute('aria-selected', active ? 'true' : 'false');
    });
    modePanels.forEach((panel) => {
      const active = panel.dataset.modePanel === next;
      panel.classList.toggle('is-active', active);
      panel.hidden = !active;
    });
    syncControls(next);
    loadActiveFrame(next);
    if (updateUrl) {
      history.replaceState(null, '', `${window.location.pathname}${window.location.search}#${next}`);
    }
  };

  modeButtons.forEach((button) => {
    button.addEventListener('click', () => setMode(button.dataset.mode));
  });

  window.addEventListener('hashchange', () => setMode(window.location.hash, { updateUrl: false }));

  const initialMode = normalizeMode(window.location.hash || params.get('mode'));
  setMode(initialMode, { updateUrl: false });
})();
