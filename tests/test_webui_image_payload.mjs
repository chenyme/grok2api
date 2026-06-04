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
  'const window = { location: { origin: "http://localhost:8000" } };',
  'let pendingFiles = [];',
  'const text = (_key, fallback) => fallback;',
  extractFunction('isImageUrl'),
  extractFunction('extractImageUrls'),
  extractFunction('normalizeImageReferenceUrl'),
  extractFunction('extractMarkdownImageUrls'),
  extractFunction('extractTextImageUrls'),
  extractFunction('extractLatestAssistantImageUrl'),
  extractFunction('imageDownloadExtension'),
  extractFunction('imageDownloadFilename'),
  extractFunction('compactStoredMediaForQuota'),
  extractFunction('promptRequestsNewImage'),
  extractFunction('restoreLegacyUserImageSummaries'),
  extractFunction('serializeMessageContentForStore'),
  extractFunction('stripUserImageBlocks'),
  extractFunction('webuiImageConfigForCapability'),
  extractFunction('buildUserMessage'),
  'return {',
  '  webuiImageConfigForCapability,',
  '  extractLatestAssistantImageUrl,',
  '  imageDownloadExtension,',
  '  imageDownloadFilename,',
  '  compactStoredMediaForQuota,',
  '  promptRequestsNewImage,',
  '  restoreLegacyUserImageSummaries,',
  '  serializeMessageContentForStore,',
  '  stripUserImageBlocks,',
  '  buildUserMessage,',
  '  setPendingFiles(value) { pendingFiles = value; },',
  '};',
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

const assistantImage = 'data:image/jpeg;base64,aW1hZ2U=';
const history = [
  { role: 'user', content: '画一张图' },
  { role: 'assistant', content: `完成了\n![image](${assistantImage})` },
];

assert.equal(helpers.extractLatestAssistantImageUrl(history), assistantImage);
assert.equal(helpers.imageDownloadExtension(assistantImage), 'jpg');
assert.equal(helpers.imageDownloadExtension('/v1/files/image?id=abc123456789abcd'), 'png');
assert.equal(helpers.imageDownloadExtension('https://example.com/output.webp?x=1'), 'webp');
assert.equal(
  helpers.imageDownloadFilename(assistantImage, new Date('2026-06-04T09:08:07.006Z')),
  'grok-image-2026-06-04T09-08-07-006Z.jpg',
);

helpers.setPendingFiles([]);
assert.deepEqual(
  helpers.buildUserMessage('改成短发', 'chat', helpers.extractLatestAssistantImageUrl(history)),
  {
    role: 'user',
    content: [
      { type: 'text', text: '改成短发' },
      { type: 'image_url', image_url: { url: assistantImage } },
    ],
  },
);

helpers.setPendingFiles([]);
assert.deepEqual(
  helpers.buildUserMessage('换成红色衣服', 'image_edit', helpers.extractLatestAssistantImageUrl(history)),
  {
    role: 'user',
    content: [
      { type: 'text', text: '换成红色衣服' },
      { type: 'image_url', image_url: { url: assistantImage } },
    ],
  },
);

assert.equal(helpers.promptRequestsNewImage('重新生成一张短发美女'), true);
helpers.setPendingFiles([]);
assert.deepEqual(
  helpers.buildUserMessage('重新生成一张短发美女', 'image', helpers.extractLatestAssistantImageUrl(history)),
  { role: 'user', content: '重新生成一张短发美女' },
);

helpers.setPendingFiles([{
  name: 'manual.png',
  type: 'image/png',
  size: 4,
  dataUrl: 'data:image/png;base64,bWFudWFs',
}]);
assert.deepEqual(
  helpers.buildUserMessage('以这张为准', 'chat', assistantImage),
  {
    role: 'user',
    content: [
      { type: 'text', text: '以这张为准' },
      { type: 'image_url', image_url: { url: 'data:image/png;base64,bWFudWFs' } },
    ],
  },
);

assert.equal(
  helpers.extractLatestAssistantImageUrl([
    { role: 'assistant', content: '完成了\n/v1/files/image?id=abc123456789abcd' },
  ]),
  'http://localhost:8000/v1/files/image?id=abc123456789abcd',
);

assert.deepEqual(
  helpers.stripUserImageBlocks([
    { type: 'text', text: '上一轮' },
    { type: 'image_url', image_url: { url: assistantImage } },
  ]),
  [{ type: 'text', text: '上一轮' }],
);

assert.deepEqual(
  helpers.serializeMessageContentForStore([
    { type: 'text', text: '做成写实风格' },
    { type: 'image_url', image_url: { url: assistantImage } },
  ], history),
  [
    { type: 'text', text: '做成写实风格' },
    { type: 'image_url', image_url: { url: 'webui:latest-assistant-image' } },
  ],
);

assert.deepEqual(
  helpers.serializeMessageContentForStore([
    { type: 'text', text: '上传参考图' },
    { type: 'image_url', image_url: { url: 'data:image/png;base64,bWFudWFs' } },
  ], history),
  [
    { type: 'text', text: '上传参考图' },
    { type: 'image_url', image_url: { url: 'data:image/png;base64,bWFudWFs' } },
  ],
);

assert.deepEqual(
  helpers.restoreLegacyUserImageSummaries([
    { role: 'assistant', content: `完成了\n![image](${assistantImage})` },
    { role: 'user', content: '做成写实风格\n\n[1 image]' },
  ]),
  [
    { role: 'assistant', content: `完成了\n![image](${assistantImage})` },
    {
      role: 'user',
      content: [
        { type: 'text', text: '做成写实风格' },
        { type: 'image_url', image_url: { url: assistantImage } },
      ],
    },
  ],
);

assert.deepEqual(
  helpers.compactStoredMediaForQuota([{
    id: 's1',
    messages: [
      { role: 'assistant', content: `完成了\n![image](${assistantImage})` },
      {
        role: 'user',
        content: [
          { type: 'text', text: '上传参考图' },
          { type: 'image_url', image_url: { url: 'data:image/png;base64,bWFudWFs' } },
        ],
      },
    ],
  }]),
  [{
    id: 's1',
    messages: [
      { role: 'assistant', content: '完成了\n[image omitted from local storage]' },
      {
        role: 'user',
        content: [
          { type: 'text', text: '上传参考图' },
          { type: 'text', text: '[image omitted from local storage]' },
        ],
      },
    ],
  }],
);

assert.deepEqual(
  helpers.restoreLegacyUserImageSummaries([
    { role: 'assistant', content: `完成了\n![image](${assistantImage})` },
    {
      role: 'user',
      content: [
        { type: 'text', text: '做成写实风格' },
        { type: 'image_url', image_url: { url: 'webui:latest-assistant-image' } },
      ],
    },
  ]),
  [
    { role: 'assistant', content: `完成了\n![image](${assistantImage})` },
    {
      role: 'user',
      content: [
        { type: 'text', text: '做成写实风格' },
        { type: 'image_url', image_url: { url: assistantImage } },
      ],
    },
  ],
);
