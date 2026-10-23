const assert = require('node:assert/strict');
const path = require('node:path');
const { test } = require('node:test');
const remarkLocalizedDocLinks = require('../plugins/remark-localized-doc-links');

const siteDir = path.resolve(__dirname, '..');

function transform(source, url, type = 'link') {
  const node = { type, url };
  const tree = {
    type: 'root',
    children: [{ type: 'paragraph', children: [node] }],
  };
  remarkLocalizedDocLinks()(tree, { path: path.join(siteDir, source) });
  return node.url;
}

for (const root of [
  'docs',
  'versioned_docs/version-v1.0.0',
  'i18n/zh-Hans/docusaurus-plugin-content-docs/current',
  'i18n/zh-Hans/docusaurus-plugin-content-docs/version-v1.0.0',
]) {
  test(`resolves links across translation boundaries from ${root}`, () => {
    assert.equal(
      transform(
        `${root}/getting-started/installation.md`,
        '../general/cert-manager.md',
      ),
      '/general/cert-manager.md',
    );
    assert.equal(
      transform(
        `${root}/general/cert-manager.md`,
        '../getting-started/installation.md#prerequisites',
      ),
      '/getting-started/installation.md#prerequisites',
    );
  });
}

test('preserves queries, anchors, MDX, and reference-style links', () => {
  assert.equal(
    transform(
      'docs/intro.md',
      './architecture/architecture.mdx?from=intro#controllers',
      'definition',
    ),
    '/architecture/architecture.mdx?from=intro#controllers',
  );
});

test('leaves URLs, assets, existing root links, and out-of-tree links unchanged', () => {
  for (const url of [
    'https://example.com/guide.md',
    '#prerequisites',
    '/getting-started/installation.md',
    '../assets/example.yaml',
    '../assets/diagram.svg',
    '../../README.md',
  ]) {
    assert.equal(transform('docs/intro.md', url), url);
  }
  assert.equal(
    transform('blog/post/index.md', '../another-post/index.md'),
    '../another-post/index.md',
  );
  assert.equal(transform('docs/intro.md', './image.md', 'image'), './image.md');
});

test('keeps missing Markdown targets subject to Docusaurus link validation', () => {
  assert.equal(transform('docs/intro.md', './missing.md'), '/missing.md');
});
