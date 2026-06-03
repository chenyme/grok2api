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

const helpers = Function([
  extractFunction('webuiImageConfigForCapability'),
  'return { webuiImageConfigForCapability };',
].join('\n'))();

assert.deepEqual(
  helpers.webuiImageConfigForCapability('image'),
  { response_format: 'local_url' },
);
assert.deepEqual(
  helpers.webuiImageConfigForCapability('image_edit'),
  { response_format: 'local_url' },
);
assert.equal(helpers.webuiImageConfigForCapability('chat'), null);
