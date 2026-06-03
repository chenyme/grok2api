import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

const source = readFileSync(new URL('../app/statics/js/webui/chat.js', import.meta.url), 'utf8');

function extractFunction(name) {
  const start = source.indexOf(`function ${name}(`);
  assert.notEqual(start, -1, `missing function ${name}`);
  const open = source.indexOf('{', start);
  let depth = 0;
  for (let i = open; i < source.length; i += 1) {
    const ch = source[i];
    if (ch === '{') depth += 1;
    if (ch === '}') depth -= 1;
    if (depth === 0) return source.slice(start, i + 1);
  }
  throw new Error(`unterminated function ${name}`);
}

const escapeHtml = (value) => String(value)
  .replace(/&/g, '&amp;')
  .replace(/</g, '&lt;')
  .replace(/>/g, '&gt;')
  .replace(/"/g, '&quot;')
  .replace(/'/g, '&#39;');

const helpers = Function('escapeHtml', [
  extractFunction('sanitizeUrl'),
  extractFunction('sanitizeSrcUrl'),
  extractFunction('isImageUrl'),
  extractFunction('isVideoUrl'),
  extractFunction('normalizeMediaContent'),
  'return { normalizeMediaContent, sanitizeUrl, sanitizeSrcUrl };',
].join('\n'))(escapeHtml);

const grokImageUrl = 'https://assets.grok.com/users/df9666a5/generated/2d973d08/image.jpg';
const normalized = helpers.normalizeMediaContent(`喜欢吗？ ${grokImageUrl}`);

assert.match(normalized, /喜欢吗？/);
assert.doesNotMatch(normalized, /!\[image\]/);
assert.match(normalized, /https:\/\/assets\.grok\.com\/users\/df9666a5\/generated\/2d973d08\/image\.jpg/);

const standalone = helpers.normalizeMediaContent(grokImageUrl);
assert.match(standalone, /!\[image\]\(https:\/\/assets\.grok\.com\/users\/df9666a5\/generated\/2d973d08\/image\.jpg\)/);

const dataImage = 'data:image/jpeg;base64,aW1hZ2UtYnl0ZXM=';
assert.equal(helpers.sanitizeUrl(dataImage), '');
assert.equal(helpers.sanitizeSrcUrl(dataImage), dataImage);
