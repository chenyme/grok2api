(() => {
  const VERIFY_ENDPOINT = '/webui/api/verify';
  const MODELS_ENDPOINT = '/webui/api/models';
  const CHAT_ENDPOINT = '/webui/api/chat/completions';
  const PREFERRED_MODEL = 'grok-4.20-0309-non-reasoning';
  const STORE_KEY = 'grok2api_webui_chat_sessions_v1';
  const SIDEBAR_STORE_KEY = 'grok2api_webui_sidebar_collapsed_v1';
  const HIDE_BUILTIN_STORE_KEY = 'grok2api_webui_hide_builtin_models_v1';
  const MEDIA_DB_NAME = 'grok2api_webui_media_v1';
  const MEDIA_DB_STORE = 'images';
  const STORED_IMAGE_PREFIX = 'webui:stored-image:';

  const chatLayout = document.getElementById('chatLayout');
  const modelSelect = document.getElementById('modelSelect');
  const modelRefreshBtn = document.getElementById('modelRefreshBtn');
  const hideBuiltinModelsBtn = document.getElementById('hideBuiltinModelsBtn');
  const systemInput = document.getElementById('systemInput');
  const thread = document.getElementById('thread');
  const emptyState = document.getElementById('emptyState');
  const statusEl = document.getElementById('status');
  const promptInput = document.getElementById('promptInput');
  const sendBtn = document.getElementById('sendBtn');
  const newChatBtn = document.getElementById('newChatBtn');
  const sidebarToggleBtn = document.getElementById('sidebarToggleBtn');
  const sessionList = document.getElementById('sessionList');
  const uploadBtn = document.getElementById('uploadBtn');
  const fileInput = document.getElementById('fileInput');
  const uploadMeta = document.getElementById('uploadMeta');
  const sessionModal = document.getElementById('sessionModal');
  const sessionModalTitle = document.getElementById('sessionModalTitle');
  const sessionModalDesc = document.getElementById('sessionModalDesc');
  const sessionModalInputWrap = document.getElementById('sessionModalInputWrap');
  const sessionModalInput = document.getElementById('sessionModalInput');
  const sessionModalCancel = document.getElementById('sessionModalCancel');
  const sessionModalConfirm = document.getElementById('sessionModalConfirm');

  let sessions = [];
  let currentSessionId = '';
  let messages = [];
  let abortController = null;
  let sending = false;
  let pendingFiles = [];
  let modalResolver = null;
  let sidebarCollapsed = false;
  let availableModels = [];
  let hideBuiltinModels = false;
  let activeEdit = null;
  const PROMPT_MIN_HEIGHT = 36;
  const PROMPT_MAX_HEIGHT = 108;
  let pendingThreadScrollFrame = 0;
  let sessionListRenderSignature = '';
  const pendingMediaWrites = new Map();
  let pendingMediaFlushFrame = 0;

  function text(key, fallback, params) {
    if (typeof window.t !== 'function') return fallback;
    const value = t(key, params);
    return value === key ? fallback : value;
  }

  function toast(message, type = 'info') {
    if (typeof showToast === 'function') showToast(message, type);
  }

  function formatModelOptionLabel(modelId, fallbackName) {
    const normalized = String(modelId || '').trim().toLowerCase();
    if (!normalized) return fallbackName || '';

    return normalized
      .split('-')
      .filter(Boolean)
      .map((part) => part ? part.charAt(0).toUpperCase() + part.slice(1) : part)
      .join(' ');
  }

  function currentSystemPrompt() {
    return systemInput ? (systemInput.value || '').trim() : '';
  }

  function escapeHtml(value) {
    return String(value)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;')
      .replace(/'/g, '&#39;');
  }

  function hasVisibleReasoning(value) {
    return typeof value === 'string' && value.trim().length > 0;
  }

  function hasMessageContent(value) {
    const textValue = typeof value === 'string' ? value : extractTextContent(value);
    return Boolean((textValue || '').trim());
  }

  function sanitizeUrl(value) {
    try {
      const url = new URL(value, window.location.origin);
      return ['http:', 'https:', 'mailto:'].includes(url.protocol) ? url.href : '';
    } catch {
      return '';
    }
  }

  function sanitizeSrcUrl(value) {
    const raw = String(value || '').trim();
    if (/^data:image\/(?:png|jpe?g|gif|webp|bmp);base64,[a-z0-9+/]+=*$/i.test(raw)) {
      return raw;
    }
    return sanitizeUrl(raw);
  }

  function sanitizeRenderedHtml(html) {
    const template = document.createElement('template');
    template.innerHTML = html;

    const blockedTags = new Set(['script', 'style', 'iframe', 'object', 'embed', 'link', 'meta']);

    function walk(node) {
      if (node.nodeType !== Node.ELEMENT_NODE) return;
      const el = node;
      const tag = el.tagName.toLowerCase();

      if (blockedTags.has(tag)) {
        el.remove();
        return;
      }

      Array.from(el.attributes).forEach((attr) => {
        const name = attr.name.toLowerCase();
        const value = attr.value || '';
        if (name.startsWith('on')) {
          el.removeAttribute(attr.name);
          return;
        }
        if (name === 'href' && !sanitizeUrl(value)) {
          el.removeAttribute(attr.name);
          return;
        }
        if (name === 'src' && !sanitizeSrcUrl(value)) {
          el.removeAttribute(attr.name);
          return;
        }
        if (name === 'target') {
          el.setAttribute('target', '_blank');
        }
      });

      Array.from(el.children).forEach((child) => walk(child));
    }

    Array.from(template.content.children).forEach((child) => walk(child));
    return template.innerHTML;
  }

  function renderInlineMarkdown(source) {
    let html = escapeHtml(source);
    html = html.replace(/`([^`]+)`/g, (_, code) => `<code>${code}</code>`);
    html = html.replace(/\[([^\]]+)\]\(([^)]+)\)/g, (_, label, href) => {
      const safeHref = sanitizeUrl(href.trim());
      const safeLabel = label.trim() || href.trim();
      return safeHref
        ? `<a href="${escapeHtml(safeHref)}" target="_blank" rel="noreferrer">${safeLabel}</a>`
        : safeLabel;
    });
    html = html.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
    html = html.replace(/(^|[^\*])\*([^*]+)\*/g, '$1<em>$2</em>');
    return html;
  }

  function renderMarkdown(source) {
    const lines = String(source || '').replace(/\r\n?/g, '\n').split('\n');
    const html = [];
    const paragraph = [];
    let listType = '';
    let listItems = [];
    let inCodeBlock = false;
    let codeLines = [];

    function flushParagraph() {
      if (!paragraph.length) return;
      html.push(`<p>${paragraph.map((line) => renderInlineMarkdown(line)).join('<br>')}</p>`);
      paragraph.length = 0;
    }

    function flushList() {
      if (!listItems.length) return;
      html.push(`<${listType}>${listItems.map((item) => `<li>${renderInlineMarkdown(item)}</li>`).join('')}</${listType}>`);
      listItems = [];
      listType = '';
    }

    function flushCodeBlock() {
      if (!inCodeBlock) return;
      html.push(`<pre><code>${escapeHtml(codeLines.join('\n'))}</code></pre>`);
      inCodeBlock = false;
      codeLines = [];
    }

    for (const line of lines) {
      if (line.startsWith('```')) {
        flushParagraph();
        flushList();
        if (inCodeBlock) {
          flushCodeBlock();
        } else {
          inCodeBlock = true;
          codeLines = [];
        }
        continue;
      }

      if (inCodeBlock) {
        codeLines.push(line);
        continue;
      }

      const trimmed = line.trim();
      const headingMatch = trimmed.match(/^(#{1,6})\s+(.*)$/);
      const unorderedMatch = trimmed.match(/^[-*+]\s+(.*)$/);
      const orderedMatch = trimmed.match(/^\d+\.\s+(.*)$/);
      const quoteMatch = trimmed.match(/^>\s?(.*)$/);

      if (!trimmed) {
        flushParagraph();
        flushList();
        continue;
      }

      if (headingMatch) {
        flushParagraph();
        flushList();
        const level = headingMatch[1].length;
        html.push(`<h${level}>${renderInlineMarkdown(headingMatch[2])}</h${level}>`);
        continue;
      }

      if (unorderedMatch || orderedMatch) {
        flushParagraph();
        const nextType = unorderedMatch ? 'ul' : 'ol';
        const itemText = unorderedMatch ? unorderedMatch[1] : orderedMatch[1];
        if (listType && listType !== nextType) flushList();
        listType = nextType;
        listItems.push(itemText);
        continue;
      }

      flushList();

      if (quoteMatch) {
        flushParagraph();
        html.push(`<blockquote>${renderInlineMarkdown(quoteMatch[1])}</blockquote>`);
        continue;
      }

      paragraph.push(line);
    }

    flushParagraph();
    flushList();
    flushCodeBlock();
    return html.join('') || '<p></p>';
  }

  function _extractMath(source) {
    const placeholders = [];
    // Display math: $$...$$ (must come before inline to avoid double-match)
    let out = source.replace(/\$\$([\s\S]+?)\$\$/g, (_, tex) => {
      const i = placeholders.length;
      placeholders.push({ tex, display: true });
      return `\x02MATH${i}\x03`;
    });
    // Inline math: $...$  (single-line only, no space at edges to avoid false positives)
    out = out.replace(/\$([^\n$]+?)\$/g, (_, tex) => {
      const i = placeholders.length;
      placeholders.push({ tex, display: false });
      return `\x02MATH${i}\x03`;
    });
    return { out, placeholders };
  }

  function renderRichMarkdown(source) {
    if (window.marked && typeof window.marked.parse === 'function') {
      let toRender = normalizeMediaContent(source);
      let placeholders = [];

      if (window.katex) {
        const extracted = _extractMath(toRender);
        toRender = extracted.out;
        placeholders = extracted.placeholders;
      }

      let rendered = window.marked.parse(toRender, {
        async: false,
        breaks: true,
        gfm: true,
      });

      if (window.katex && placeholders.length) {
        rendered = rendered.replace(/\x02MATH(\d+)\x03/g, (_, idx) => {
          const { tex, display } = placeholders[parseInt(idx, 10)];
          try {
            return window.katex.renderToString(tex, { displayMode: display, throwOnError: false });
          } catch (_e) {
            return escapeHtml(display ? `$$${tex}$$` : `$${tex}$`);
          }
        });
      }

      return sanitizeRenderedHtml(rendered);
    }
    return renderMarkdown(source);
  }

  function isImageUrl(value) {
    const normalized = String(value || '').trim().toLowerCase();
    return normalized.includes('/v1/files/image')
      || /\.(png|jpe?g|gif|webp|bmp|svg)(\?|#|$)/.test(normalized)
      || normalized.startsWith('data:image/')
      || normalized.startsWith(STORED_IMAGE_PREFIX);
  }

  function isVideoUrl(value) {
    const normalized = String(value || '').trim().toLowerCase();
    return normalized.includes('/v1/files/video')
      || /\.(mp4|webm|mov|m4v|ogg)(\?|#|$)/.test(normalized);
  }

  function normalizeMediaContent(source) {
    const input = String(source || '').replace(/\[video\]\(([^)]+)\)/gi, '$1');
    return input.replace(/^(https?:\/\/\S+|\/v1\/files\/(?:image|video)\?id=\S+|data:image\/[^\s]+)$/gm, (match) => {
      const url = match.trim();
      if (isImageUrl(url)) return `![image](${url})`;
      if (isVideoUrl(url)) return `<video controls preload="metadata" src="${escapeHtml(url)}"></video>`;
      return match;
    });
  }

  function isNativeGrokMediaUrl(value) {
    try {
      const url = new URL(value, window.location.origin);
      return /(^|\.)grok\.com$/i.test(url.hostname);
    } catch {
      return false;
    }
  }

  function showMediaProxyHint(media, type) {
    if (!media || media.nextElementSibling?.classList?.contains('msg-media-error')) return;
    const hint = document.createElement('div');
    hint.className = 'msg-media-error';
    if (type === 'image') {
      hint.textContent = text(
        'webui.chat.errors.imageProxyRequired',
        'Image failed to load. Set APP Base URL and change image output format to local_url, local_md, or base64.'
      );
    } else {
      hint.textContent = text(
        'webui.chat.errors.videoProxyRequired',
        'Video loading returned 403. Go to the admin page, set the APP Base URL, then change the video output format to local proxy mode (local_url or local_html) and retry.'
      );
    }
    media.insertAdjacentElement('afterend', hint);
  }

  function clearMediaProxyHint(media) {
    const hint = media && media.nextElementSibling;
    if (hint?.classList?.contains('msg-media-error')) hint.remove();
  }

  function enhanceMediaElements(card) {
    card.querySelectorAll('video').forEach((video) => {
      if (video.dataset.proxyHintBound === '1') return;
      video.dataset.proxyHintBound = '1';
      const onVideoError = () => showMediaProxyHint(video, 'video');
      video.addEventListener('error', onVideoError);
      video.querySelectorAll('source').forEach((source) => {
        source.addEventListener('error', onVideoError);
      });
      video.addEventListener('loadedmetadata', () => clearMediaProxyHint(video));
      if (video.error) showMediaProxyHint(video, 'video');
    });

    card.querySelectorAll('img').forEach((img) => {
      if (img.dataset.proxyHintBound === '1') return;
      img.dataset.proxyHintBound = '1';
      img.addEventListener('error', () => {
        if (isNativeGrokMediaUrl(img.currentSrc || img.src)) showMediaProxyHint(img, 'image');
      });
      img.addEventListener('load', () => clearMediaProxyHint(img));
      if (img.complete && img.naturalWidth === 0 && isNativeGrokMediaUrl(img.currentSrc || img.src)) {
        showMediaProxyHint(img, 'image');
      }
    });
  }

  function extractTextContent(content) {
    if (typeof content === 'string') return content;
    if (!Array.isArray(content)) return '';
    return content
      .filter((block) => block && block.type === 'text' && typeof block.text === 'string' && block.text.trim())
      .map((block) => block.text.trim())
      .join('\n');
  }

  function extractImageUrls(content) {
    if (!Array.isArray(content)) return [];
    return content.flatMap((block) => {
      if (!block || block.type !== 'image_url') return [];
      const image = block.image_url;
      if (typeof image === 'string' && image.trim()) return [image.trim()];
      if (image && typeof image.url === 'string' && image.url.trim()) return [image.url.trim()];
      return [];
    });
  }

  function normalizeImageReferenceUrl(value) {
    const raw = String(value || '').trim();
    if (!raw) return '';
    if (raw.startsWith('data:image/')) return raw;
    if (raw.startsWith(STORED_IMAGE_PREFIX)) return raw;
    try {
      return new URL(raw, window.location.origin).href;
    } catch {
      return raw;
    }
  }

  function extractMarkdownImageUrls(source) {
    const textValue = String(source || '');
    const urls = [];
    const markdownRe = /!\[[^\]]*\]\(([^)\s]+)\)/gi;
    for (let match = markdownRe.exec(textValue); match; match = markdownRe.exec(textValue)) {
      const url = normalizeImageReferenceUrl(match[1]);
      if (url && isImageUrl(url)) urls.push(url);
    }
    return urls;
  }

  function extractTextImageUrls(source) {
    const textValue = String(source || '');
    const urls = extractMarkdownImageUrls(textValue);
    const urlRe = /(https?:\/\/[^\s<>)]+|\/v1\/files\/image\?id=[^\s<>)]+|data:image\/[^\s<>)]+|webui:stored-image:[a-z0-9-]+)/gi;
    for (let match = urlRe.exec(textValue); match; match = urlRe.exec(textValue)) {
      const url = normalizeImageReferenceUrl(match[1]);
      if (url && isImageUrl(url) && !urls.includes(url)) urls.push(url);
    }
    return urls;
  }

  function extractLatestAssistantImageUrl(history) {
    for (let index = (history || []).length - 1; index >= 0; index -= 1) {
      const message = history[index];
      if (!message || message.role !== 'assistant') continue;
      const urls = Array.isArray(message.content)
        ? extractImageUrls(message.content).map(normalizeImageReferenceUrl).filter(Boolean)
        : extractTextImageUrls(message.content);
      if (urls.length) return urls[urls.length - 1];
    }
    return '';
  }

  function imageDownloadExtension(url) {
    const value = String(url || '').trim().toLowerCase();
    const dataMatch = value.match(/^data:image\/([^;,]+)/);
    if (dataMatch) {
      const mimeType = dataMatch[1];
      if (mimeType === 'jpeg') return 'jpg';
      if (mimeType === 'svg+xml') return 'svg';
      return mimeType.split('+')[0] || 'png';
    }
    const pathMatch = value.match(/\.([a-z0-9]+)(?:[?#]|$)/);
    if (pathMatch) {
      const ext = pathMatch[1] === 'jpeg' ? 'jpg' : pathMatch[1];
      if (['png', 'jpg', 'gif', 'webp', 'bmp', 'svg'].includes(ext)) return ext;
    }
    if (value.includes('/v1/files/image')) return 'png';
    return 'png';
  }

  function imageDownloadFilename(url, now = new Date()) {
    const stamp = now.toISOString().replace(/[:.]/g, '-');
    return `grok-image-${stamp}.${imageDownloadExtension(url)}`;
  }

  function assistantEntryImageUrl(entry) {
    if (!entry) return '';
    const message = entry.messageIndex >= 0 ? messages[entry.messageIndex] : null;
    const content = message && message.content !== undefined ? message.content : entry.text;
    return extractLatestAssistantImageUrl([{ role: 'assistant', content }]);
  }

  function clickDownloadLink(href, filename) {
    const link = document.createElement('a');
    link.href = href;
    link.download = filename;
    link.rel = 'noreferrer';
    link.style.display = 'none';
    document.body.appendChild(link);
    link.click();
    link.remove();
  }

  async function downloadImageUrl(url) {
    const href = normalizeImageReferenceUrl(url);
    if (!href) return false;
    if (href.startsWith(STORED_IMAGE_PREFIX)) {
      const dataUrl = await readStoredImageDataUrl(href);
      if (!dataUrl) return false;
      clickDownloadLink(dataUrl, imageDownloadFilename(dataUrl));
      return true;
    }
    const filename = imageDownloadFilename(href);
    if (href.startsWith('data:image/')) {
      clickDownloadLink(href, filename);
      return true;
    }
    try {
      const parsed = new URL(href, window.location.origin);
      const sameOrigin = parsed.origin === window.location.origin;
      const response = await fetch(parsed.href, { credentials: sameOrigin ? 'same-origin' : 'omit' });
      if (!response.ok) throw new Error(`HTTP ${response.status}`);
      const blob = await response.blob();
      const objectUrl = URL.createObjectURL(blob);
      clickDownloadLink(objectUrl, filename);
      window.setTimeout(() => URL.revokeObjectURL(objectUrl), 1000);
      return true;
    } catch (_error) {
      clickDownloadLink(href, filename);
      return true;
    }
  }

  function promptRequestsNewImage(prompt) {
    const normalized = String(prompt || '').trim().toLowerCase();
    if (!normalized) return false;
    return /(?:重新|从头|全新|新图|新图片|新的图|新的图片|新生成|生成新的|画一张新的|换一张新的|不要基于上一张|不基于上一张|不要沿用|不要参考上一张|new image|new picture|start over|from scratch)/i.test(normalized);
  }

  function stripUserImageBlocks(content) {
    if (!Array.isArray(content)) return content;
    const filtered = content.filter((block) => !block || block.type !== 'image_url');
    if (!filtered.length) return '';
    return filtered;
  }

  function extractFileItems(content) {
    if (!Array.isArray(content)) return [];
    return content.flatMap((block) => {
      if (!block || typeof block !== 'object') return [];
      if (block.type === 'input_audio') {
        const audio = block.input_audio || {};
        const filename = String(audio.filename || '').trim();
        return [{ kind: 'audio', name: filename || 'audio' }];
      }
      if (block.type === 'file') {
        const file = block.file || {};
        const filename = String(file.filename || '').trim();
        return [{ kind: 'file', name: filename || 'file' }];
      }
      return [];
    });
  }

  function dataUrlMime(value) {
    const match = String(value || '').match(/^data:([^;,]+)[;,]/i);
    return match ? match[1].toLowerCase() : 'application/octet-stream';
  }

  function fallbackNameForMime(mime) {
    if (mime.startsWith('image/')) return `image.${mime.split('/')[1] || 'png'}`;
    if (mime.startsWith('audio/')) return `audio.${mime.split('/')[1] || 'wav'}`;
    return `file.${mime.split('/')[1] || 'bin'}`;
  }

  function extractEditablePendingFiles(content) {
    if (!Array.isArray(content)) return [];
    return content.flatMap((block) => {
      if (!block || typeof block !== 'object') return [];
      if (block.type === 'image_url') {
        const image = block.image_url;
        const url = typeof image === 'string' ? image : image && typeof image.url === 'string' ? image.url : '';
        if (!url || !url.startsWith('data:')) return [];
        const mime = dataUrlMime(url);
        return [{
          name: fallbackNameForMime(mime),
          type: mime,
          size: 0,
          dataUrl: url,
        }];
      }
      if (block.type === 'input_audio') {
        const audio = block.input_audio || {};
        const data = String(audio.data || '').trim();
        if (!data) return [];
        const mime = dataUrlMime(data);
        return [{
          name: String(audio.filename || '').trim() || fallbackNameForMime(mime),
          type: mime,
          size: 0,
          dataUrl: data,
        }];
      }
      if (block.type === 'file') {
        const file = block.file || {};
        const data = String(file.file_data || '').trim();
        if (!data) return [];
        const mime = dataUrlMime(data);
        return [{
          name: String(file.filename || '').trim() || fallbackNameForMime(mime),
          type: mime,
          size: 0,
          dataUrl: data,
        }];
      }
      return [];
    });
  }

  async function copyToClipboard(value) {
    const textValue = String(value || '');
    if (!textValue) return;
    if (navigator.clipboard && typeof navigator.clipboard.writeText === 'function') {
      await navigator.clipboard.writeText(textValue);
      return;
    }
    const textarea = document.createElement('textarea');
    textarea.value = textValue;
    textarea.setAttribute('readonly', '');
    textarea.style.position = 'fixed';
    textarea.style.opacity = '0';
    document.body.appendChild(textarea);
    textarea.select();
    document.execCommand('copy');
    textarea.remove();
  }

  function beginEditMessage(messageIndex, content) {
    activeEdit = {
      messageIndex,
      text: extractTextContent(content) || (typeof content === 'string' ? content : ''),
      files: extractEditablePendingFiles(content),
    };
    renderThread();
  }

  function summarizeMessageContent(content) {
    const textContent = extractTextContent(content).trim();
    const imageCount = extractImageUrls(content).length;
    const fileCount = extractFileItems(content).length;
    const parts = [];
    if (textContent) parts.push(textContent);
    if (imageCount) parts.push(`[${imageCount} image${imageCount > 1 ? 's' : ''}]`);
    if (fileCount) parts.push(`[${fileCount} file${fileCount > 1 ? 's' : ''}]`);
    return parts.join('\n\n');
  }

  function storedImageReference(dataUrl) {
    const value = String(dataUrl || '');
    let hash = 5381;
    for (let index = 0; index < value.length; index += 1) {
      hash = ((hash << 5) + hash + value.charCodeAt(index)) >>> 0;
    }
    return `${STORED_IMAGE_PREFIX}${value.length.toString(36)}-${hash.toString(36)}`;
  }

  function scheduleMediaStoreFlush() {
    if (pendingMediaFlushFrame || !pendingMediaWrites.size) return;
    const flush = () => {
      pendingMediaFlushFrame = 0;
      void flushPendingMediaWrites();
    };
    if (typeof window.requestIdleCallback === 'function') {
      pendingMediaFlushFrame = window.requestIdleCallback(flush, { timeout: 1500 });
    } else if (typeof window.setTimeout === 'function') {
      pendingMediaFlushFrame = window.setTimeout(flush, 0);
    }
  }

  function queueStoredImageDataUrl(dataUrl) {
    const value = String(dataUrl || '');
    const ref = storedImageReference(value);
    if (value.startsWith('data:image/')) {
      pendingMediaWrites.set(ref, value);
      scheduleMediaStoreFlush();
    }
    return ref;
  }

  function openMediaStore() {
    if (!window.indexedDB) return Promise.reject(new Error('IndexedDB unavailable'));
    return new Promise((resolve, reject) => {
      const request = window.indexedDB.open(MEDIA_DB_NAME, 1);
      request.onupgradeneeded = () => {
        const db = request.result;
        if (!db.objectStoreNames.contains(MEDIA_DB_STORE)) db.createObjectStore(MEDIA_DB_STORE, { keyPath: 'id' });
      };
      request.onsuccess = () => resolve(request.result);
      request.onerror = () => reject(request.error || new Error('IndexedDB open failed'));
    });
  }

  async function flushPendingMediaWrites() {
    if (!pendingMediaWrites.size) return;
    const writes = Array.from(pendingMediaWrites.entries());
    pendingMediaWrites.clear();
    let db;
    try {
      db = await openMediaStore();
      await new Promise((resolve, reject) => {
        const tx = db.transaction(MEDIA_DB_STORE, 'readwrite');
        const store = tx.objectStore(MEDIA_DB_STORE);
        writes.forEach(([id, dataUrl]) => store.put({ id, dataUrl, updatedAt: Date.now() }));
        tx.oncomplete = resolve;
        tx.onerror = () => reject(tx.error || new Error('IndexedDB write failed'));
        tx.onabort = () => reject(tx.error || new Error('IndexedDB write aborted'));
      });
    } catch (_error) {
      writes.forEach(([id, dataUrl]) => pendingMediaWrites.set(id, dataUrl));
    } finally {
      if (db) db.close();
    }
  }

  async function readStoredImageDataUrl(ref) {
    if (!String(ref || '').startsWith(STORED_IMAGE_PREFIX)) return '';
    let db;
    try {
      db = await openMediaStore();
      return await new Promise((resolve) => {
        const tx = db.transaction(MEDIA_DB_STORE, 'readonly');
        const request = tx.objectStore(MEDIA_DB_STORE).get(ref);
        request.onsuccess = () => resolve(request.result && request.result.dataUrl ? request.result.dataUrl : '');
        request.onerror = () => resolve('');
      });
    } catch {
      return '';
    } finally {
      if (db) db.close();
    }
  }

  async function hydrateStoredImageRefsInContent(content) {
    const refRe = /webui:stored-image:[a-z0-9-]+/gi;
    if (typeof content === 'string') {
      const refs = Array.from(new Set(content.match(refRe) || []));
      let hydrated = content;
      for (const ref of refs) {
        const dataUrl = await readStoredImageDataUrl(ref);
        if (dataUrl) hydrated = hydrated.split(ref).join(dataUrl);
      }
      return hydrated;
    }
    if (!Array.isArray(content)) return content;
    const next = [];
    for (const block of content) {
      if (!block || typeof block !== 'object') {
        next.push(block);
        continue;
      }
      if (block.type === 'text' && typeof block.text === 'string') {
        next.push({ ...block, text: await hydrateStoredImageRefsInContent(block.text) });
        continue;
      }
      if (block.type === 'image_url') {
        const image = block.image_url;
        const url = typeof image === 'string' ? image : image && typeof image.url === 'string' ? image.url : '';
        if (url.startsWith(STORED_IMAGE_PREFIX)) {
          const dataUrl = await readStoredImageDataUrl(url);
          next.push(dataUrl ? { ...block, image_url: { url: dataUrl } } : block);
          continue;
        }
      }
      next.push(block);
    }
    return next;
  }

  async function hydrateStoredImageRefsInSessions(sessionsList) {
    for (const session of sessionsList || []) {
      for (const message of session.messages || []) {
        message.content = await hydrateStoredImageRefsInContent(message.content);
      }
    }
  }

  function serializeTextMediaForStore(textValue) {
    return String(textValue || '').replace(/data:image\/[^\s<>)]+/gi, (match) => queueStoredImageDataUrl(match));
  }

  function serializeMessageContentForStore(content, previousMessages = []) {
    if (typeof content === 'string') return serializeTextMediaForStore(content);
    if (!Array.isArray(content)) return content;
    const latestAssistantImage = extractLatestAssistantImageUrl(previousMessages);
    return content.map((block) => {
      if (!block || typeof block !== 'object') return block;
      const copy = JSON.parse(JSON.stringify(block));
      if (copy.type === 'text' && typeof copy.text === 'string') {
        copy.text = serializeTextMediaForStore(copy.text);
      }
      if (copy.type === 'image_url') {
        const image = copy.image_url;
        const url = typeof image === 'string' ? image : image && typeof image.url === 'string' ? image.url : '';
        if (url && url.startsWith('data:image/') && latestAssistantImage && url === latestAssistantImage) {
          copy.image_url = { url: 'webui:latest-assistant-image' };
        } else if (url && url.startsWith('data:image/')) {
          copy.image_url = { url: queueStoredImageDataUrl(url) };
        }
      }
      return copy;
    });
  }

  function serializeMessageForStore(message, previousMessages = []) {
    return {
      ...message,
      content: serializeMessageContentForStore(message && message.content, previousMessages),
    };
  }

  function restoreLegacyUserImageSummaries(messagesList) {
    const restored = [];
    (messagesList || []).forEach((entry) => {
      if (entry && entry.role === 'user' && Array.isArray(entry.content)) {
        const imageUrl = extractLatestAssistantImageUrl(restored);
        if (imageUrl) {
          const content = entry.content.map((block) => {
            if (!block || block.type !== 'image_url') return block;
            const image = block.image_url;
            const url = typeof image === 'string' ? image : image && typeof image.url === 'string' ? image.url : '';
            if (url !== 'webui:latest-assistant-image') return block;
            return { ...block, image_url: { url: imageUrl } };
          });
          restored.push({ ...entry, content });
          return;
        }
      }
      if (
        entry
        && entry.role === 'user'
        && typeof entry.content === 'string'
        && /(?:^|\n)\s*\[1 image\]\s*$/i.test(entry.content)
      ) {
        const imageUrl = extractLatestAssistantImageUrl(restored);
        if (imageUrl) {
          const textContent = entry.content.replace(/(?:^|\n)\s*\[1 image\]\s*$/i, '').trim();
          restored.push({
            ...entry,
            content: [
              ...(textContent ? [{ type: 'text', text: textContent }] : []),
              { type: 'image_url', image_url: { url: imageUrl } },
            ],
          });
          return;
        }
      }
      restored.push(entry);
    });
    return restored;
  }

  function compactStoredMediaForQuota(sessionsForStore) {
    const compactValue = (value) => {
      if (typeof value === 'string') return serializeTextMediaForStore(value);
      if (Array.isArray(value)) {
        return value.map((item) => compactValue(item));
      }
      if (value && typeof value === 'object') {
        return Object.fromEntries(Object.entries(value).map(([key, item]) => [key, compactValue(item)]));
      }
      return value;
    };
    return compactValue(sessionsForStore);
  }

  function renderMessageContent(card, role, content) {
    if (Array.isArray(content)) {
      const textContent = extractTextContent(content);
      const imageUrls = extractImageUrls(content);
      const fileItems = extractFileItems(content);
      if (role === 'assistant') {
        const parts = [];
        if (textContent.trim()) parts.push(renderRichMarkdown(textContent));
        if (imageUrls.length) {
          parts.push(imageUrls.map((url) => (
            `<div class="msg-inline-media"><img src="${escapeHtml(url)}" alt="image" loading="lazy"></div>`
          )).join(''));
        }
        card.innerHTML = parts.join('') || '<p></p>';
        enhanceMediaElements(card);
        return;
      }

      const body = document.createElement('div');
      body.className = 'msg-user-parts';
      if (textContent.trim()) {
        const textNode = document.createElement('div');
        textNode.className = 'msg-user-text';
        textNode.textContent = textContent;
        body.appendChild(textNode);
      }
      if (imageUrls.length) {
        const gallery = document.createElement('div');
        gallery.className = 'msg-user-gallery';
        imageUrls.forEach((url) => {
          const img = document.createElement('img');
          img.src = url;
          img.alt = 'image';
          img.loading = 'lazy';
          gallery.appendChild(img);
        });
        body.appendChild(gallery);
      }
      if (fileItems.length) {
        const attachments = document.createElement('div');
        attachments.className = 'msg-user-files';
        fileItems.forEach((item) => {
          const chip = document.createElement('div');
          chip.className = 'msg-user-file';
          chip.textContent = item.name;
          attachments.appendChild(chip);
        });
        body.appendChild(attachments);
      }
      card.replaceChildren(body);
      return;
    }

    if (role === 'assistant') {
      card.innerHTML = renderRichMarkdown(content);
      enhanceMediaElements(card);
      return;
    }
    card.textContent = content;
  }

  function renderAssistantWaiting(card) {
    card.innerHTML = '<div class="msg-loading" aria-hidden="true"><span class="msg-loading-spinner"></span></div>';
  }

  function parseSseEvent(chunk) {
    let event = 'message';
    const dataLines = [];
    for (const line of chunk.split('\n')) {
      if (line.startsWith('event:')) {
        event = line.slice(6).trim() || 'message';
        continue;
      }
      if (line.startsWith('data:')) {
        dataLines.push(line.slice(5).trimStart());
      }
    }
    return { event, data: dataLines.join('\n') };
  }

  function loadStore() {
    try {
      const raw = localStorage.getItem(STORE_KEY);
      if (!raw) return { sessions: [], currentSessionId: '' };
      const parsed = JSON.parse(raw);
      if (Array.isArray(parsed)) return { sessions: parsed, currentSessionId: parsed[0] && parsed[0].id || '' };
      return {
        sessions: Array.isArray(parsed && parsed.sessions) ? parsed.sessions : [],
        currentSessionId: parsed && parsed.currentSessionId ? String(parsed.currentSessionId) : '',
      };
    } catch {
      return { sessions: [], currentSessionId: '' };
    }
  }

  function persistStore() {
    const serializedSessions = sessions.map((session) => ({
      ...session,
      messages: Array.isArray(session.messages)
        ? session.messages.map((message, index) => serializeMessageForStore(message, session.messages.slice(0, index)))
        : [],
    }));
    try {
      localStorage.setItem(STORE_KEY, JSON.stringify({ sessions: serializedSessions, currentSessionId }));
      void flushPendingMediaWrites();
    } catch (_error) {
      try {
        const compactSessions = compactStoredMediaForQuota(serializedSessions);
        localStorage.setItem(STORE_KEY, JSON.stringify({ sessions: compactSessions, currentSessionId }));
        void flushPendingMediaWrites();
        toast(
          text('webui.chat.errors.historyStorageCompacted', 'Browser storage is full; image data was compacted in saved history.'),
          'error',
        );
      } catch (compactError) {
        toast(compactError.message || String(compactError), 'error');
      }
    }
  }

  function applySidebarState() {
    if (!chatLayout || !sidebarToggleBtn) return;
    chatLayout.classList.toggle('sidebar-collapsed', sidebarCollapsed);
    sidebarToggleBtn.setAttribute('aria-expanded', String(!sidebarCollapsed));
  }

  function loadSidebarState() {
    try {
      sidebarCollapsed = localStorage.getItem(SIDEBAR_STORE_KEY) === 'true';
    } catch {
      sidebarCollapsed = false;
    }
    applySidebarState();
  }

  function toggleSidebar() {
    sidebarCollapsed = !sidebarCollapsed;
    applySidebarState();
    try {
      localStorage.setItem(SIDEBAR_STORE_KEY, String(sidebarCollapsed));
    } catch {}
  }

  function loadModelFilterState() {
    try {
      hideBuiltinModels = localStorage.getItem(HIDE_BUILTIN_STORE_KEY) === 'true';
    } catch {
      hideBuiltinModels = false;
    }
  }

  function persistModelFilterState() {
    try {
      localStorage.setItem(HIDE_BUILTIN_STORE_KEY, String(hideBuiltinModels));
    } catch {}
  }

  function syncModelFilterButton() {
    if (!hideBuiltinModelsBtn) return;
    hideBuiltinModelsBtn.setAttribute('aria-pressed', hideBuiltinModels ? 'true' : 'false');
    const label = hideBuiltinModels
      ? text('webui.chat.showBuiltinModels', '显示内置模型')
      : text('webui.chat.hideBuiltinModels', '隐藏内置模型');
    hideBuiltinModelsBtn.textContent = label;
    hideBuiltinModelsBtn.title = label;
  }

  function createSessionTitle(messagesList) {
    const firstUser = messagesList.find((item) => {
      if (!item || item.role !== 'user') return false;
      return Boolean(extractTextContent(item.content).trim());
    });
    if (!firstUser) return text('webui.chat.untitled', 'New Chat');
    const trimmed = extractTextContent(firstUser.content).trim().replace(/\s+/g, ' ');
    return trimmed.length > 24 ? `${trimmed.slice(0, 24)}...` : trimmed;
  }

  function createSession() {
    return {
      id: `chat_${Date.now()}_${Math.random().toString(36).slice(2, 8)}`,
      title: text('webui.chat.untitled', 'New Chat'),
      titleLocked: false,
      model: modelSelect.value || PREFERRED_MODEL,
      system: '',
      messages: [],
      updatedAt: Date.now(),
    };
  }

  function normalizeSession(item) {
    return {
      id: item && item.id ? String(item.id) : `chat_${Date.now()}_${Math.random().toString(36).slice(2, 8)}`,
      title: item && item.title ? String(item.title) : text('webui.chat.untitled', 'New Chat'),
      titleLocked: Boolean(item && item.titleLocked),
      model: item && item.model ? String(item.model) : PREFERRED_MODEL,
      system: item && item.system ? String(item.system) : '',
      messages: restoreLegacyUserImageSummaries(Array.isArray(item && item.messages)
        ? item.messages
          .filter((entry) => {
            if (!entry || typeof entry.role !== 'string') return false;
            if (!['user', 'assistant', 'error'].includes(entry.role)) return false;
            return typeof entry.content === 'string' || Array.isArray(entry.content);
          })
          .map((entry) => ({
            ...entry,
            reasoning_content: entry && entry.role === 'assistant' && hasVisibleReasoning(entry.reasoning_content)
              ? entry.reasoning_content
              : '',
            createdAt: Number(entry && entry.createdAt) || Date.now(),
            feedback: entry && typeof entry.feedback === 'string' ? entry.feedback : '',
          }))
        : []),
      updatedAt: Number(item && item.updatedAt) || Date.now(),
    };
  }

  function setAssistantFeedback(messageIndex, feedback) {
    const session = getCurrentSession();
    const message = session && session.messages && session.messages[messageIndex];
    if (!session || !message || message.role !== 'assistant') return;
    message.feedback = message.feedback === feedback ? '' : feedback;
    session.updatedAt = Date.now();
    persistStore();
    renderThread();
  }

  function regenerateAssistantAt(messageIndex) {
    const session = getCurrentSession();
    if (!session || sending || messageIndex < 0) return;

    let userIndex = -1;
    for (let index = messageIndex - 1; index >= 0; index -= 1) {
      if (messages[index] && messages[index].role === 'user') {
        userIndex = index;
        break;
      }
    }
    if (userIndex < 0) return;

    const userContent = messages[userIndex].content;
    promptInput.value = extractTextContent(userContent) || (typeof userContent === 'string' ? userContent : '');
    pendingFiles = extractEditablePendingFiles(userContent);
    messages = messages.slice(0, userIndex);
    session.messages = messages;
    session.updatedAt = Date.now();
    activeEdit = null;
    renderUploadMeta();
    renderSessionList();
    renderThread();
    resizePromptInput();
    void sendMessage();
  }

  function getCurrentSession() {
    return sessions.find((item) => item.id === currentSessionId) || null;
  }

  function moveSessionToTop(session) {
    sessions = [session, ...sessions.filter((item) => item.id !== session.id)];
  }

  async function getAuthHeaders() {
    const key = await webuiKey.get();
    return key ? { Authorization: `Bearer ${key}` } : {};
  }

  async function ensureAccess() {
    const stored = await webuiKey.get();
    if (stored && await verifyKey(VERIFY_ENDPOINT, stored)) return true;
    if (stored) webuiKey.clear();
    if (await verifyKey(VERIFY_ENDPOINT, '')) return true;
    location.href = '/webui/login';
    return false;
  }

  function setStatus(textValue) {
    if (statusEl) statusEl.textContent = textValue;
  }

  function resizePromptInput() {
    if (!promptInput) return;
    promptInput.style.height = `${PROMPT_MIN_HEIGHT}px`;
    const nextHeight = Math.min(Math.max(promptInput.scrollHeight, PROMPT_MIN_HEIGHT), PROMPT_MAX_HEIGHT);
    promptInput.style.height = `${nextHeight}px`;
    promptInput.style.overflowY = promptInput.scrollHeight > PROMPT_MAX_HEIGHT ? 'auto' : 'hidden';
  }

  function renderSendButton() {
    if (!sendBtn) return;
    const label = sending
      ? text('webui.chat.stop', 'Stop')
      : text('webui.chat.send', 'Send');
    sendBtn.removeAttribute('data-i18n');
    sendBtn.setAttribute('aria-label', label);
    sendBtn.setAttribute('title', label);
    sendBtn.innerHTML = sending
      ? '<svg viewBox="0 0 24 24" fill="none" aria-hidden="true"><path d="M8 8H16V16H8Z"/></svg>'
      : '<svg viewBox="0 0 24 24" fill="none" aria-hidden="true"><path d="M5 12H19"/><path d="M13 6L19 12L13 18"/></svg>';
  }

  function setSending(next) {
    sending = next;
    promptInput.disabled = next;
    modelSelect.disabled = next;
    if (modelRefreshBtn) modelRefreshBtn.disabled = next;
    if (hideBuiltinModelsBtn) hideBuiltinModelsBtn.disabled = next;
    if (systemInput) systemInput.disabled = next;
    renderSendButton();
  }

  function scrollThread() {
    if (pendingThreadScrollFrame) return;
    pendingThreadScrollFrame = window.requestAnimationFrame(() => {
      pendingThreadScrollFrame = 0;
      thread.scrollTop = thread.scrollHeight;
    });
  }

  function hideEmpty() {
    if (emptyState) emptyState.style.display = 'none';
  }

  function showEmpty() {
    if (emptyState) emptyState.style.display = '';
  }

  function renderUploadMeta() {
    if (!uploadMeta) return;
    if (!pendingFiles.length) {
      uploadMeta.hidden = true;
      uploadMeta.replaceChildren();
      return;
    }

    const row = document.createElement('div');
    row.className = 'webui-upload-meta-row';

    pendingFiles.forEach((file, index) => {
      const chip = document.createElement('div');
      chip.className = 'webui-upload-meta-chip';
      chip.title = file.name || 'file';
      const chars = Array.from(String(file.name || 'file'));

      const label = document.createElement('span');
      label.className = 'webui-upload-meta-chip-label';
      label.textContent = chars.length > 5 ? `${chars.slice(0, 5).join('')}...` : (file.name || 'file');
      chip.appendChild(label);

      const removeBtn = document.createElement('button');
      removeBtn.type = 'button';
      removeBtn.className = 'webui-upload-meta-chip-remove';
      removeBtn.setAttribute('aria-label', `删除 ${file.name || 'file'}`);
      removeBtn.setAttribute('title', `删除 ${file.name || 'file'}`);
      removeBtn.innerHTML = '<svg viewBox="0 0 24 24" fill="none" aria-hidden="true"><path d="M8 8L16 16M16 8L8 16" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"/></svg>';
      removeBtn.addEventListener('click', () => {
        pendingFiles = pendingFiles.filter((_, itemIndex) => itemIndex !== index);
        if (fileInput && !pendingFiles.length) fileInput.value = '';
        renderUploadMeta();
      });
      chip.appendChild(removeBtn);

      row.appendChild(chip);
    });

    uploadMeta.hidden = false;
    uploadMeta.replaceChildren(row);
  }

  function currentModelCapability() {
    const selected = modelSelect && modelSelect.value
      ? availableModels.find((item) => item && item.id === modelSelect.value)
      : null;
    return selected && selected.capability ? selected.capability : 'chat';
  }

  function webuiImageConfigForCapability(capability) {
    if (capability === 'chat') return { response_format: 'local_md' };
    return capability === 'image' || capability === 'image_edit'
      ? { response_format: 'local_url' }
      : null;
  }

  async function fileToDataUrl(file) {
    return await new Promise((resolve, reject) => {
      const reader = new FileReader();
      reader.onload = () => resolve(String(reader.result || ''));
      reader.onerror = () => reject(reader.error || new Error('file read failed'));
      reader.readAsDataURL(file);
    });
  }

  async function preparePendingFiles(fileList) {
    const files = Array.from(fileList || []);
    const prepared = [];

    for (const file of files) {
      if (!file) continue;
      prepared.push({
        name: file.name || 'file',
        type: file.type || 'application/octet-stream',
        size: Number(file.size) || 0,
        dataUrl: await fileToDataUrl(file),
      });
    }

    return prepared;
  }

  function buildUserMessage(prompt, capability, contextImageUrl = '') {
    const textBlock = prompt ? [{ type: 'text', text: prompt }] : [];
    const imageFiles = pendingFiles.filter((file) => (file.type || '').startsWith('image/'));
    const audioFiles = pendingFiles.filter((file) => (file.type || '').startsWith('audio/'));
    const otherFiles = pendingFiles.filter((file) => {
      const mime = file.type || '';
      return !mime.startsWith('image/') && !mime.startsWith('audio/');
    });

    const imageBlocks = imageFiles.map((file) => ({
      type: 'image_url',
      image_url: { url: file.dataUrl },
    }));
    const contextImage = !imageBlocks.length && contextImageUrl
      ? [{ type: 'image_url', image_url: { url: contextImageUrl } }]
      : [];
    const effectiveImageBlocks = imageBlocks.length ? imageBlocks : contextImage;
    const audioBlocks = audioFiles.map((file) => ({
      type: 'input_audio',
      input_audio: {
        data: file.dataUrl,
        filename: file.name,
      },
    }));
    const fileBlocks = otherFiles.map((file) => ({
      type: 'file',
      file: {
        file_data: file.dataUrl,
        filename: file.name,
      },
    }));

    if (capability === 'image') {
      if (pendingFiles.length) {
        throw new Error(text(
          'webui.chat.errors.imageUploadsNotSupported',
          'Image generation does not accept uploaded references here. Use chat, image edit, or video with a reference image.',
        ));
      }
      return { role: 'user', content: prompt };
    }
    if (capability === 'image_edit') {
      if (!effectiveImageBlocks.length) {
        throw new Error(text('webui.chat.errors.imageRequired', 'Image edit requires at least one reference image'));
      }
      if (audioBlocks.length || fileBlocks.length) {
        throw new Error(text('webui.chat.errors.imageOnly', 'Image edit only supports image uploads'));
      }
      return { role: 'user', content: [...textBlock, ...effectiveImageBlocks] };
    }
    if (capability === 'video') {
      if (audioBlocks.length || fileBlocks.length) {
        throw new Error(text('webui.chat.errors.videoImageOnly', 'Video generation only supports image reference uploads'));
      }
      return imageBlocks.length
        ? { role: 'user', content: [...textBlock, imageBlocks[0]] }
        : { role: 'user', content: prompt };
    }
    if (imageBlocks.length || audioBlocks.length || fileBlocks.length) {
      return { role: 'user', content: [...textBlock, ...imageBlocks, ...audioBlocks, ...fileBlocks] };
    }
    if (capability === 'chat' && contextImage.length) {
      return { role: 'user', content: [...textBlock, ...contextImage] };
    }
    return { role: 'user', content: prompt };
  }

  function closeSessionModal(result) {
    if (!sessionModal) return;
    sessionModal.classList.remove('open');
    sessionModal.setAttribute('aria-hidden', 'true');
    const resolver = modalResolver;
    modalResolver = null;
    if (resolver) resolver(result);
  }

  function openSessionModal({ title, description = '', confirmLabel, cancelLabel, inputValue = '', withInput = false }) {
    if (!sessionModal) return Promise.resolve(null);
    sessionModalTitle.textContent = title;
    sessionModalDesc.textContent = description;
    sessionModalInputWrap.hidden = !withInput;
    sessionModalInput.value = withInput ? inputValue : '';
    sessionModalCancel.textContent = cancelLabel || text('webui.chat.cancel', 'Cancel');
    sessionModalConfirm.textContent = confirmLabel || text('webui.chat.confirm', 'Confirm');
    sessionModal.classList.add('open');
    sessionModal.setAttribute('aria-hidden', 'false');
    if (withInput) {
      setTimeout(() => {
        sessionModalInput.focus();
        sessionModalInput.select();
      }, 0);
    }
    return new Promise((resolve) => {
      modalResolver = resolve;
    });
  }

  function editMessageAt(messageIndex, content) {
    const session = getCurrentSession();
    if (!session || messageIndex < 0) return;
    if (sending) stopMessage();

    promptInput.value = activeEdit ? activeEdit.text : (extractTextContent(content) || (typeof content === 'string' ? content : ''));
    pendingFiles = activeEdit ? activeEdit.files.slice() : extractEditablePendingFiles(content);
    messages = messages.slice(0, messageIndex);
    session.messages = messages;
    session.model = modelSelect.value || PREFERRED_MODEL;
    session.system = currentSystemPrompt();
    if (!session.titleLocked) session.title = createSessionTitle(session.messages);
    session.updatedAt = Date.now();
    activeEdit = null;
    moveSessionToTop(session);
    renderUploadMeta();
    renderSessionList();
    renderThread();
    resizePromptInput();
    setStatus(text('webui.chat.statusReady', 'Ready'));
    persistStore();
    promptInput.focus();
  }

  function createMessage(role, initialText = '', initialReasoning = '', messageIndex = -1) {
    hideEmpty();
    const hasReasoning = role === 'assistant' && hasVisibleReasoning(initialReasoning);
    const isAssistantWaiting = role === 'assistant' && messageIndex < 0 && !hasReasoning && !hasMessageContent(initialText);

    const wrap = document.createElement('div');
    wrap.className = `msg ${role}`;

    const reasoning = document.createElement('div');
    reasoning.className = 'msg-reasoning';
    reasoning.hidden = !hasReasoning;

    const reasoningToggle = document.createElement('button');
    reasoningToggle.type = 'button';
    reasoningToggle.className = 'msg-reasoning-toggle';
    reasoningToggle.setAttribute('aria-expanded', 'true');
    reasoningToggle.innerHTML = `<span class="msg-reasoning-label">${escapeHtml(text('webui.chat.reasoning', 'Reasoning'))}</span><svg viewBox="0 0 16 16" fill="none" aria-hidden="true"><path d="M4 6.5 8 10l4-3.5" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/></svg>`;

    const reasoningBody = document.createElement('div');
    reasoningBody.className = 'msg-reasoning-body';
    reasoningBody.textContent = hasReasoning ? initialReasoning : '';

    reasoningToggle.addEventListener('click', () => {
      const collapsed = reasoning.classList.toggle('is-collapsed');
      reasoningToggle.setAttribute('aria-expanded', collapsed ? 'false' : 'true');
    });

    reasoning.appendChild(reasoningToggle);
    reasoning.appendChild(reasoningBody);

    const card = document.createElement('div');
    card.className = `msg-card msg-card-${role}`;
    const isEditing = role === 'user' && activeEdit && activeEdit.messageIndex === messageIndex;
    if (isEditing) {
      card.classList.add('msg-card-editing');

      const editor = document.createElement('textarea');
      editor.className = 'msg-edit-textarea';
      editor.value = activeEdit.text;
      editor.placeholder = text('webui.chat.editPlaceholder', 'Edit message');
      editor.addEventListener('input', () => {
        if (!activeEdit || activeEdit.messageIndex !== messageIndex) return;
        activeEdit.text = editor.value;
        editor.style.height = 'auto';
        editor.style.height = `${Math.max(editor.scrollHeight, 52)}px`;
      });
      editor.style.height = 'auto';
      editor.style.height = `${Math.max(editor.scrollHeight, 52)}px`;

      const footer = document.createElement('div');
      footer.className = 'msg-edit-footer';

      const cancelBtn = document.createElement('button');
      cancelBtn.type = 'button';
      cancelBtn.className = 'msg-edit-cancel';
      cancelBtn.textContent = text('webui.chat.cancel', 'Cancel');
      cancelBtn.addEventListener('click', () => {
        activeEdit = null;
        renderThread();
      });

      const saveBtn = document.createElement('button');
      saveBtn.type = 'button';
      saveBtn.className = 'msg-edit-save';
      saveBtn.textContent = text('webui.chat.save', 'Save');
      saveBtn.addEventListener('click', () => {
        editMessageAt(messageIndex, initialText);
      });

      footer.appendChild(cancelBtn);
      footer.appendChild(saveBtn);
      card.appendChild(editor);
      card.appendChild(footer);

      setTimeout(() => {
        editor.focus();
        editor.setSelectionRange(editor.value.length, editor.value.length);
      }, 0);
    } else if (isAssistantWaiting) {
      renderAssistantWaiting(card);
    } else {
      renderMessageContent(card, role, initialText);
    }

    const entry = {
      wrap,
      reasoning,
      reasoningBody,
      card,
      text: initialText,
      reasoningText: initialReasoning,
      waiting: isAssistantWaiting,
      messageIndex,
      actions: null,
      likeBtn: null,
      dislikeBtn: null,
      renderFrame: 0,
    };

    if (role === 'assistant') {
      wrap.appendChild(reasoning);
    }
    wrap.appendChild(card);

    if (role === 'user') {
      const actions = document.createElement('div');
      actions.className = 'msg-actions';

      const editBtn = document.createElement('button');
      editBtn.type = 'button';
      editBtn.className = 'msg-action-btn';
      editBtn.setAttribute('aria-label', text('webui.chat.edit', 'Edit'));
      editBtn.setAttribute('title', text('webui.chat.edit', 'Edit'));
      editBtn.innerHTML = '<svg viewBox="0 0 24 24" fill="none" aria-hidden="true"><path d="M4 20h4l10-10-4-4L4 16v4Z" stroke="currentColor" stroke-width="1.8" stroke-linejoin="round"/><path d="m12.5 7.5 4 4" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"/></svg>';
      editBtn.addEventListener('click', () => {
        beginEditMessage(messageIndex, initialText);
      });

      const copyBtn = document.createElement('button');
      copyBtn.type = 'button';
      copyBtn.className = 'msg-action-btn';
      copyBtn.setAttribute('aria-label', text('webui.chat.copy', 'Copy'));
      copyBtn.setAttribute('title', text('webui.chat.copy', 'Copy'));
      copyBtn.innerHTML = '<svg viewBox="0 0 24 24" fill="none" aria-hidden="true"><rect x="9" y="9" width="10" height="10" rx="3" stroke="currentColor" stroke-width="1.8"/><path d="M15 9V8a3 3 0 0 0-3-3H8a3 3 0 0 0-3 3v4a3 3 0 0 0 3 3h1" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"/></svg>';
      copyBtn.addEventListener('click', async () => {
        try {
          await copyToClipboard(extractTextContent(initialText) || (typeof initialText === 'string' ? initialText : ''));
          toast(text('webui.chat.copySuccess', 'Copied'), 'info');
        } catch (error) {
          toast(error.message || String(error), 'error');
        }
      });

      if (!isEditing) {
        actions.appendChild(editBtn);
        actions.appendChild(copyBtn);
        wrap.appendChild(actions);
      }
    }

    if (role === 'assistant') {
      const actions = document.createElement('div');
      actions.className = 'msg-actions msg-actions-assistant';
      actions.hidden = messageIndex < 0;
      const message = messageIndex >= 0 ? messages[messageIndex] : null;

      const right = document.createElement('div');
      right.className = 'msg-action-group';

      const regenBtn = document.createElement('button');
      regenBtn.type = 'button';
      regenBtn.className = 'msg-action-btn msg-action-btn-regen';
      regenBtn.setAttribute('aria-label', text('webui.chat.regenerate', 'Regenerate'));
      regenBtn.setAttribute('title', text('webui.chat.regenerate', 'Regenerate'));
      regenBtn.innerHTML = '<svg viewBox="0 0 24 24" fill="none" aria-hidden="true"><path d="M21 2v6h-6" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"/><path d="M3 11a9 9 0 0 1 15.3-6.3L21 8" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"/><path d="M3 22v-6h6" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"/><path d="M21 13a9 9 0 0 1-15.3 6.3L3 16" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"/></svg>';
      regenBtn.addEventListener('click', () => {
        regenerateAssistantAt(entry.messageIndex);
      });

      const copyBtn = document.createElement('button');
      copyBtn.type = 'button';
      copyBtn.className = 'msg-action-btn';
      copyBtn.setAttribute('aria-label', text('webui.chat.copy', 'Copy'));
      copyBtn.setAttribute('title', text('webui.chat.copy', 'Copy'));
      copyBtn.innerHTML = '<svg viewBox="0 0 24 24" fill="none" aria-hidden="true"><rect x="9" y="9" width="10" height="10" rx="3" stroke="currentColor" stroke-width="1.7"/><path d="M15 9V8a3 3 0 0 0-3-3H8a3 3 0 0 0-3 3v4a3 3 0 0 0 3 3h1" stroke="currentColor" stroke-width="1.7" stroke-linecap="round"/></svg>';
      copyBtn.addEventListener('click', async () => {
        try {
          await copyToClipboard(typeof entry.text === 'string' ? entry.text : extractTextContent(entry.text));
          toast(text('webui.chat.copySuccess', 'Copied'), 'info');
        } catch (error) {
          toast(error.message || String(error), 'error');
        }
      });

      const downloadBtn = document.createElement('button');
      downloadBtn.type = 'button';
      downloadBtn.className = 'msg-action-btn';
      downloadBtn.hidden = true;
      downloadBtn.setAttribute('aria-label', text('webui.chat.downloadImage', 'Download image'));
      downloadBtn.setAttribute('title', text('webui.chat.downloadImage', 'Download image'));
      downloadBtn.innerHTML = '<svg viewBox="0 0 24 24" fill="none" aria-hidden="true"><path d="M12 4v10" stroke="currentColor" stroke-width="1.7" stroke-linecap="round"/><path d="m8 10 4 4 4-4" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"/><path d="M5 20h14" stroke="currentColor" stroke-width="1.7" stroke-linecap="round"/></svg>';
      downloadBtn.addEventListener('click', async () => {
        try {
          const imageUrl = assistantEntryImageUrl(entry);
          if (!imageUrl) {
            toast(text('webui.chat.errors.noImageToDownload', 'No image to download'), 'error');
            return;
          }
          await downloadImageUrl(imageUrl);
        } catch (error) {
          toast(error.message || String(error), 'error');
        }
      });

      const likeBtn = document.createElement('button');
      likeBtn.type = 'button';
      likeBtn.className = `msg-action-btn${message && message.feedback === 'up' ? ' active' : ''}`;
      likeBtn.setAttribute('aria-label', text('webui.chat.like', 'Like'));
      likeBtn.setAttribute('title', text('webui.chat.like', 'Like'));
      likeBtn.innerHTML = '<svg viewBox="0 0 24 24" fill="none" aria-hidden="true"><path d="M7 11.5v7.5M10.5 19h6.1a1.8 1.8 0 0 0 1.76-1.44l1.12-5.6A1.8 1.8 0 0 0 17.72 10H14V6.9a1.7 1.7 0 0 0-3.12-.93L7 11.5v7.5" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"/></svg>';
      likeBtn.addEventListener('click', () => {
        setAssistantFeedback(entry.messageIndex, 'up');
      });

      const dislikeBtn = document.createElement('button');
      dislikeBtn.type = 'button';
      dislikeBtn.className = `msg-action-btn${message && message.feedback === 'down' ? ' active' : ''}`;
      dislikeBtn.setAttribute('aria-label', text('webui.chat.dislike', 'Dislike'));
      dislikeBtn.setAttribute('title', text('webui.chat.dislike', 'Dislike'));
      dislikeBtn.innerHTML = '<svg viewBox="0 0 24 24" fill="none" aria-hidden="true"><path d="M7 12.5V5M10.5 5h6.1a1.8 1.8 0 0 1 1.76 1.44l1.12 5.6A1.8 1.8 0 0 1 17.72 14H14v3.1a1.7 1.7 0 0 1-3.12.93L7 12.5V5" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"/></svg>';
      dislikeBtn.addEventListener('click', () => {
        setAssistantFeedback(entry.messageIndex, 'down');
      });

      right.appendChild(regenBtn);
      right.appendChild(copyBtn);
      right.appendChild(downloadBtn);
      right.appendChild(likeBtn);
      right.appendChild(dislikeBtn);
      actions.appendChild(right);
      wrap.appendChild(actions);
      entry.actions = actions;
      entry.downloadBtn = downloadBtn;
      entry.likeBtn = likeBtn;
      entry.dislikeBtn = dislikeBtn;
    }

    thread.appendChild(wrap);

    syncAssistantActions(entry);
    return entry;
  }

  function syncAssistantActions(entry) {
    if (!entry || !entry.actions) return;
    entry.actions.hidden = entry.messageIndex < 0;
    const message = entry.messageIndex >= 0 ? messages[entry.messageIndex] : null;
    if (entry.downloadBtn) entry.downloadBtn.hidden = !assistantEntryImageUrl(entry);
    if (entry.likeBtn) entry.likeBtn.classList.toggle('active', Boolean(message && message.feedback === 'up'));
    if (entry.dislikeBtn) entry.dislikeBtn.classList.toggle('active', Boolean(message && message.feedback === 'down'));
  }

  function renderAssistantEntry(entry) {
    if (!entry) return;
    entry.renderFrame = 0;
    if (entry.waiting) return;
    if (hasMessageContent(entry.text)) {
      renderMessageContent(entry.card, 'assistant', entry.text);
    } else {
      entry.card.innerHTML = '';
    }
    const hasReasoning = hasVisibleReasoning(entry.reasoningText);
    entry.reasoning.hidden = !hasReasoning;
    entry.reasoningBody.textContent = hasReasoning ? entry.reasoningText : '';
  }

  function scheduleAssistantEntryRender(entry) {
    if (!entry) return;
    if (!entry.renderFrame) {
      entry.renderFrame = window.requestAnimationFrame(() => {
        renderAssistantEntry(entry);
        scrollThread();
      });
    } else {
      scrollThread();
    }
  }

  function flushAssistantEntry(entry) {
    if (!entry) return;
    if (entry.renderFrame) {
      window.cancelAnimationFrame(entry.renderFrame);
      entry.renderFrame = 0;
    }
    renderAssistantEntry(entry);
  }

  function finalizeAssistantEntry(entry, messageIndex) {
    if (!entry) return;
    entry.waiting = false;
    flushAssistantEntry(entry);
    entry.messageIndex = messageIndex;
    syncAssistantActions(entry);
    scrollThread();
  }

  function updateAssistant(entry, delta) {
    if (entry.waiting) entry.waiting = false;
    entry.text += delta;
    scheduleAssistantEntryRender(entry);
  }

  function updateReasoning(entry, delta) {
    if (entry.waiting) entry.waiting = false;
    entry.reasoningText += delta;
    scheduleAssistantEntryRender(entry);
  }

  function renderThread() {
    thread.innerHTML = '';
    if (emptyState) thread.appendChild(emptyState);
    if (!messages.length) {
      showEmpty();
      return;
    }
    hideEmpty();
    messages.forEach((message, index) => {
      createMessage(
        message.role,
        message.content,
        message.role === 'assistant' ? (message.reasoning_content || '') : '',
        index,
      );
    });
    scrollThread();
  }

  function renderSessionList() {
    if (!sessionList) return;
    sessionList.dataset.empty = text('webui.chat.noSessions', 'No chats yet');
    const nextSignature = `${currentSessionId}|${sessions.map((session) => `${session.id}:${session.title || ''}`).join('|')}`;
    if (nextSignature === sessionListRenderSignature) return;
    sessionListRenderSignature = nextSignature;
    const fragment = document.createDocumentFragment();

    sessions.forEach((session) => {
      const item = document.createElement('button');
      item.type = 'button';
      item.className = `webui-session-item${session.id === currentSessionId ? ' active' : ''}`;

      const title = document.createElement('div');
      title.className = 'webui-session-title';
      title.textContent = session.title || text('webui.chat.untitled', 'New Chat');
      const actions = document.createElement('div');
      actions.className = 'webui-session-actions';

      const renameBtn = document.createElement('button');
      renameBtn.type = 'button';
      renameBtn.className = 'webui-session-action';
      renameBtn.title = text('webui.chat.rename', 'Rename');
      renameBtn.innerHTML = '<svg viewBox="0 0 24 24" fill="none"><path d="M4 20h4l10-10-4-4L4 16v4Z" stroke="currentColor" stroke-width="1.8" stroke-linejoin="round"/><path d="m12.5 7.5 4 4" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"/></svg>';
      renameBtn.addEventListener('click', (event) => {
        event.stopPropagation();
        renameSession(session.id);
      });

      const deleteBtn = document.createElement('button');
      deleteBtn.type = 'button';
      deleteBtn.className = 'webui-session-action';
      deleteBtn.title = text('webui.chat.delete', 'Delete');
      deleteBtn.innerHTML = '<svg viewBox="0 0 24 24" fill="none"><path d="M5 7h14M9 7V5h6v2M8 7l1 12h6l1-12" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"/></svg>';
      deleteBtn.addEventListener('click', (event) => {
        event.stopPropagation();
        deleteSession(session.id);
      });

      actions.appendChild(renameBtn);
      actions.appendChild(deleteBtn);

      item.appendChild(title);
      item.appendChild(actions);
      item.addEventListener('click', () => switchSession(session.id));
      fragment.appendChild(item);
    });
    sessionList.replaceChildren(fragment);
  }

  function syncCurrentSession() {
    const session = getCurrentSession();
    if (!session) return;
    session.model = modelSelect.value || PREFERRED_MODEL;
    session.system = currentSystemPrompt();
    if (!session.titleLocked) session.title = createSessionTitle(session.messages);
    session.updatedAt = Date.now();
    moveSessionToTop(session);
    persistStore();
    renderSessionList();
  }

  function switchSession(id) {
    const session = sessions.find((item) => item.id === id);
    if (!session) return;
    currentSessionId = session.id;
    messages = session.messages;
    pendingFiles = [];
    activeEdit = null;
    if (modelSelect.options.length) {
      modelSelect.value = Array.from(modelSelect.options).some((option) => option.value === session.model)
        ? session.model
        : (modelSelect.value || PREFERRED_MODEL);
    }
    renderUploadMeta();
    renderSessionList();
    renderThread();
    resizePromptInput();
    setStatus(text('webui.chat.statusReady', 'Ready'));
    persistStore();
  }

  function startNewSession() {
    const session = createSession();
    sessions.unshift(session);
    currentSessionId = session.id;
    messages = session.messages;
    pendingFiles = [];
    activeEdit = null;
    renderUploadMeta();
    renderSessionList();
    renderThread();
    resizePromptInput();
    setStatus(text('webui.chat.statusReady', 'Ready'));
    persistStore();
    promptInput.focus();
  }

  function renameSession(id) {
    const session = sessions.find((item) => item.id === id);
    if (!session) return;
    openSessionModal({
      title: text('webui.chat.rename', 'Rename'),
      description: text('webui.chat.renamePrompt', 'Rename session'),
      confirmLabel: text('webui.chat.confirm', 'Confirm'),
      cancelLabel: text('webui.chat.cancel', 'Cancel'),
      inputValue: session.title || text('webui.chat.untitled', 'New Chat'),
      withInput: true,
    }).then((nextTitle) => {
      if (typeof nextTitle !== 'string') return;
      const trimmed = nextTitle.trim();
      if (!trimmed) return;
      session.title = trimmed;
      session.titleLocked = true;
      session.updatedAt = Date.now();
      moveSessionToTop(session);
      persistStore();
      renderSessionList();
    });
  }

  function deleteSession(id) {
    const session = sessions.find((item) => item.id === id);
    if (!session) return;
    openSessionModal({
      title: text('webui.chat.delete', 'Delete'),
      description: text('webui.chat.deleteConfirm', 'Delete this session?'),
      confirmLabel: text('webui.chat.delete', 'Delete'),
      cancelLabel: text('webui.chat.cancel', 'Cancel'),
    }).then((confirmed) => {
      if (!confirmed) return;
      sessions = sessions.filter((item) => item.id !== id);
      if (!sessions.length) {
        startNewSession();
        return;
      }

      const next = sessions[0];
      currentSessionId = next.id;
      persistStore();
      switchSession(next.id);
    });
  }

  function buildPayload() {
    const outgoing = [];
    const system = currentSystemPrompt();
    if (system) outgoing.push({ role: 'system', content: system });
    const lastUserIndex = (() => {
      for (let index = messages.length - 1; index >= 0; index -= 1) {
        if (messages[index]?.role === 'user') return index;
      }
      return -1;
    })();
    const lastUserHasImage = lastUserIndex >= 0 && extractImageUrls(messages[lastUserIndex].content).length > 0;
    messages
      .filter((message) => message && (message.role === 'user' || message.role === 'assistant'))
      .forEach((message) => {
        const originalIndex = messages.indexOf(message);
        if (
          lastUserHasImage
          && message.role === 'user'
          && originalIndex !== lastUserIndex
        ) {
          outgoing.push({ ...message, content: stripUserImageBlocks(message.content) });
          return;
        }
        outgoing.push(message);
      });
    const payload = {
      model: modelSelect.value || PREFERRED_MODEL,
      messages: outgoing,
      stream: true,
      temperature: 0.8,
      top_p: 0.95,
    };
    const imageConfig = webuiImageConfigForCapability(currentModelCapability());
    if (imageConfig) payload.image_config = imageConfig;
    return payload;
  }

  function visibleModels() {
    return hideBuiltinModels
      ? availableModels.filter((item) => item && item.manual)
      : availableModels;
  }

  function renderModelOptions(previous = '') {
    const models = visibleModels();
    const ids = models.map((item) => item && item.id).filter(Boolean);
    modelSelect.innerHTML = '';
    models.forEach((item) => {
      const opt = document.createElement('option');
      opt.value = item.id;
      const label = formatModelOptionLabel(item.id, item.name || item.id);
      opt.textContent = item.manual ? `${label}（手工）` : label;
      if (item.manual) {
        opt.classList.add('webui-model-option-manual');
        opt.dataset.source = item.source || 'manual';
      }
      modelSelect.appendChild(opt);
    });
    if (previous && ids.includes(previous)) {
      modelSelect.value = previous;
    } else {
      modelSelect.value = ids.includes(PREFERRED_MODEL) ? PREFERRED_MODEL : (ids[0] || PREFERRED_MODEL);
    }
    syncModelFilterButton();
  }

  async function loadModels(options = {}) {
    const previous = options.preserve ? modelSelect.value : '';
    const headers = await getAuthHeaders();
    const res = await fetch(MODELS_ENDPOINT, { headers, cache: 'no-store' });
    if (!res.ok) throw new Error(`models ${res.status}`);

    const data = await res.json();
    const items = Array.isArray(data && data.data) ? data.data : [];
    availableModels = items.filter((item) => item && item.id);
    renderModelOptions(previous);
  }

  async function sendMessage() {
    if (sending) return;

    const prompt = (promptInput.value || '').trim();
    const capability = currentModelCapability();
    if (!prompt) {
      toast(text('webui.chat.errors.enterPrompt', 'Please enter a message'), 'error');
      return;
    }

    const session = getCurrentSession();
    if (!session) return;
    activeEdit = null;

    let userMessage;
    try {
      const contextImageUrl = promptRequestsNewImage(prompt)
        ? ''
        : extractLatestAssistantImageUrl(messages);
      userMessage = buildUserMessage(prompt, capability, contextImageUrl);
    } catch (error) {
      toast(error.message || String(error), 'error');
      return;
    }

    session.model = modelSelect.value || PREFERRED_MODEL;
    session.system = currentSystemPrompt();
    messages.push(userMessage);
    if (!session.titleLocked) session.title = createSessionTitle(messages);
    session.updatedAt = Date.now();
    moveSessionToTop(session);
    persistStore();
    renderSessionList();

    messages[messages.length - 1].createdAt = Date.now();
    messages[messages.length - 1].feedback = '';
    const userEntry = createMessage('user', userMessage.content, '', messages.length - 1);
    void userEntry;
    const assistantCreatedAt = Date.now();
    const assistantEntry = createMessage('assistant', '', '', -1);

    promptInput.value = '';
    pendingFiles = [];
    if (fileInput) fileInput.value = '';
    renderUploadMeta();
    resizePromptInput();
    abortController = new AbortController();
    setSending(true);
    setStatus(text('webui.chat.statusConnecting', 'Connecting...'));

    try {
      const headers = {
        'Content-Type': 'application/json',
        ...(await getAuthHeaders()),
      };
      const res = await fetch(CHAT_ENDPOINT, {
        method: 'POST',
        headers,
        body: JSON.stringify(buildPayload()),
        signal: abortController.signal,
      });
      if (!res.ok) {
        const detail = await res.text().catch(() => '');
        throw new Error(detail || `HTTP ${res.status}`);
      }

      const reader = res.body.getReader();
      const decoder = new TextDecoder('utf-8');
      let buffer = '';

      function handleStreamChunk(chunk) {
        const messageEvent = parseSseEvent(chunk);
        const payload = messageEvent.data.trim();
        if (!payload) return false;
        if (payload === '[DONE]') {
          const finalReasoning = hasVisibleReasoning(assistantEntry.reasoningText) ? assistantEntry.reasoningText : '';
          messages.push({
            role: 'assistant',
            content: assistantEntry.text,
            reasoning_content: finalReasoning,
            createdAt: assistantCreatedAt,
            feedback: '',
          });
          syncCurrentSession();
          finalizeAssistantEntry(assistantEntry, messages.length - 1);
          setStatus(text('webui.chat.statusDone', 'Completed'));
          return true;
        }

        let json;
        try {
          json = JSON.parse(payload);
        } catch {
          return false;
        }

        if (messageEvent.event === 'error' || json.error) {
          const errorMessage = json.error && json.error.message
            ? json.error.message
            : text('webui.chat.errors.requestFailed', 'Request failed');
          throw new Error(errorMessage);
        }

        const choice = json && json.choices && json.choices[0];
        const delta = choice && choice.delta ? choice.delta : {};
        if (typeof delta.reasoning_content === 'string') {
          updateReasoning(assistantEntry, delta.reasoning_content);
          if (hasVisibleReasoning(assistantEntry.reasoningText)) {
            setStatus(text('webui.chat.statusThinking', 'Thinking...'));
          }
        }
        if (delta.content) {
          updateAssistant(assistantEntry, delta.content);
          setStatus(text('webui.chat.statusGenerating', 'Generating...'));
        }
        return false;
      }

      while (true) {
        const { value, done } = await reader.read();
        if (done) break;

        buffer += decoder.decode(value, { stream: true }).replace(/\r\n/g, '\n');
        const chunks = buffer.split('\n\n');
        buffer = chunks.pop() || '';

        for (const chunk of chunks) {
          if (handleStreamChunk(chunk)) return;
        }
      }

      if (buffer.trim() && handleStreamChunk(buffer)) return;

      const finalReasoning = hasVisibleReasoning(assistantEntry.reasoningText) ? assistantEntry.reasoningText : '';
      messages.push({
        role: 'assistant',
        content: assistantEntry.text,
        reasoning_content: finalReasoning,
        createdAt: assistantCreatedAt,
        feedback: '',
      });
      syncCurrentSession();
      finalizeAssistantEntry(assistantEntry, messages.length - 1);
      setStatus(text('webui.chat.statusDone', 'Completed'));
    } catch (error) {
      if (error && error.name === 'AbortError') {
        setStatus(text('webui.chat.statusStopped', 'Stopped'));
      } else {
        messages.push({
          role: 'error',
          content: `${text('webui.chat.errors.requestFailed', 'Request failed')}: ${error.message || error}`,
          createdAt: Date.now(),
          feedback: '',
        });
        syncCurrentSession();
        renderThread();
        toast(text('webui.chat.errors.requestFailed', 'Request failed'), 'error');
        setStatus(text('webui.chat.statusFailed', 'Failed'));
      }
    } finally {
      abortController = null;
      setSending(false);
      scrollThread();
    }
  }

  function stopMessage() {
    if (abortController) abortController.abort();
  }

  async function restoreSessions() {
    const stored = loadStore();
    sessions = stored.sessions.map(normalizeSession);
    await hydrateStoredImageRefsInSessions(sessions);
    currentSessionId = stored.currentSessionId;

    if (!sessions.length) {
      startNewSession();
      return;
    }

    const existing = sessions.find((item) => item.id === currentSessionId) || sessions[0];
    switchSession(existing.id);
  }

  async function boot() {
    await renderWebuiHeader?.();
    await renderSiteFooter?.();
    if (window.I18n?.apply) I18n.apply(document);
    renderSendButton();
    window.I18n?.onReady?.(renderSendButton);
    if (!await ensureAccess()) return;
    loadSidebarState();
    loadModelFilterState();
    syncModelFilterButton();
    await loadModels();
    await restoreSessions();
    resizePromptInput();
    promptInput.focus();
  }

  newChatBtn?.addEventListener('click', startNewSession);
  sidebarToggleBtn.addEventListener('click', toggleSidebar);
  sendBtn.addEventListener('click', () => {
    if (sending) {
      stopMessage();
      return;
    }
    sendMessage();
  });
  modelSelect.addEventListener('change', syncCurrentSession);
  hideBuiltinModelsBtn?.addEventListener('click', () => {
    if (sending) return;
    hideBuiltinModels = !hideBuiltinModels;
    persistModelFilterState();
    renderModelOptions(modelSelect.value);
    syncCurrentSession();
  });
  modelRefreshBtn?.addEventListener('click', async () => {
    if (sending) return;
    modelRefreshBtn.disabled = true;
    try {
      await loadModels({ preserve: true });
      syncCurrentSession();
      toast(text('webui.chat.modelsRefreshed', 'Models refreshed'), 'success');
    } catch (error) {
      toast(`${text('webui.chat.errors.requestFailed', 'Request failed')}: ${error.message}`, 'error');
    } finally {
      modelRefreshBtn.disabled = false;
    }
  });
  systemInput?.addEventListener('change', syncCurrentSession);
  uploadBtn.addEventListener('click', () => fileInput.click());
  fileInput.addEventListener('change', async () => {
    try {
      pendingFiles = await preparePendingFiles(fileInput.files || []);
      renderUploadMeta();
    } catch (error) {
      pendingFiles = [];
      if (fileInput) fileInput.value = '';
      renderUploadMeta();
      toast(error.message || String(error), 'error');
    }
  });
  sessionModalCancel.addEventListener('click', () => closeSessionModal(false));
  sessionModalConfirm.addEventListener('click', () => {
    const result = sessionModalInputWrap.hidden ? true : sessionModalInput.value;
    closeSessionModal(result);
  });
  sessionModal.addEventListener('click', (event) => {
    if (event.target === sessionModal) closeSessionModal(false);
  });
  sessionModalInput.addEventListener('keydown', (event) => {
    if (event.key === 'Enter') {
      event.preventDefault();
      closeSessionModal(sessionModalInput.value);
    }
  });
  promptInput.addEventListener('input', resizePromptInput);
  promptInput.addEventListener('keydown', (event) => {
    if (event.key === 'Enter' && !event.shiftKey) {
      event.preventDefault();
      sendMessage();
    }
  });

  boot().catch((error) => {
    console.error('webui chat boot failed', error);
    toast(text('webui.chat.errors.initFailed', 'Chat page initialization failed'), 'error');
    setStatus(text('webui.chat.statusInitFailed', 'Initialization failed'));
  });
})();
