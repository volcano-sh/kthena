import assert from 'node:assert/strict';
import { existsSync, readFileSync } from 'node:fs';
import { test } from 'node:test';

const versions = JSON.parse(
  readFileSync(new URL('../versions.json', import.meta.url), 'utf8'),
);
const [defaultVersion] = versions;

// Check the actual output of the production build, including plugin-generated
// routes and theme overrides. Run after `npm run build` (or via make test-docs).
function page(route) {
  const file = route.endsWith('/') ? `${route}index.html` : `${route}.html`;
  return readFileSync(new URL(`../build${file}`, import.meta.url), 'utf8');
}

function tags(html, name) {
  return html.match(new RegExp(`<${name}\\b[^>]*>`, 'g')) ?? [];
}

function attribute(tag, name) {
  const match = tag.match(
    new RegExp(`\\s${name}=(?:"([^"]*)"|'([^']*)'|([^\\s>]+))`),
  );
  return match?.[1] ?? match?.[2] ?? match?.[3];
}

function hasLink(html, href, language) {
  return tags(html, 'a').some(
    (tag) =>
      attribute(tag, 'href') === href &&
      (!language || attribute(tag, 'lang') === language),
  );
}

test('both homepages render translated content and lead to the default release', () => {
  const en = page('/');
  const zh = page('/zh-Hans/');
  assert.equal(attribute(tags(en, 'html')[0], 'lang'), 'en');
  assert.equal(attribute(tags(zh, 'html')[0], 'lang'), 'zh-Hans');
  assert.match(en, /Get Started with Kthena/);
  assert.match(zh, /开始使用 Kthena/);
  assert.match(zh, /智能路由/);
  assert.match(zh, /分层 PD 分离编排/);
  assert.match(zh, new RegExp(`版权所有 © ${new Date().getFullYear()}`));
  assert.ok(hasLink(en, '/docs/intro'));
  assert.ok(hasLink(zh, '/zh-Hans/docs/intro'));
  assert.ok(hasLink(en, '/zh-Hans/', 'zh-Hans'));
  assert.ok(hasLink(zh, '/', 'en'));
});

for (const version of ['current', ...versions]) {
  const prefix =
    version === 'current'
      ? 'next/'
      : version === defaultVersion
        ? ''
        : `${version}/`;
  const route = `/docs/${prefix}intro`;
  const sourceVersion =
    version === 'current' ? 'current' : `version-${version}`;
  test(`Kthena introduction is translated: ${version}`, () => {
    const en = page(route);
    const zh = page(`/zh-Hans${route}`);
    assert.match(zh, /核心特性/);
    assert.doesNotMatch(zh, /此页面尚未翻译/);
    assert.ok(hasLink(en, `/zh-Hans${route}`, 'zh-Hans'));
    assert.ok(hasLink(zh, route, 'en'));
    const headings = (html) =>
      tags(html, 'h[2-6]').map((tag) => attribute(tag, 'id'));
    assert.deepEqual(headings(zh), headings(en));
    assert.ok(
      hasLink(
        zh,
        `https://github.com/volcano-sh/kthena/tree/main/docs/kthena/i18n/zh-Hans/docusaurus-plugin-content-docs/${sourceVersion}/intro.md`,
      ),
    );
  });
}

const fallbackDocs = [
  { doc: 'getting-started/installation' },
  {
    doc: 'getting-started/quick-start',
  },
  { doc: 'architecture/architecture', slug: 'architecture', extension: '.mdx' },
  { doc: 'architecture/autoscaler', extension: '.mdx' },
  { doc: 'architecture/kthena-router' },
  { doc: 'architecture/model-serving-controller', extension: '.mdx' },
  { doc: 'developer-guide/development-setup' },
  { doc: 'developer-guide/release' },
  { doc: 'general/cert-manager' },
  {
    doc: 'general/data-parallel-deployment',
  },
  {
    doc: 'user-guide/model-deployment',
  },
  {
    doc: 'user-guide/runtime',
  },
  { doc: 'user-guide/multi-node-inference' },
  { doc: 'user-guide/autoscaler' },
  { doc: 'user-guide/binpack-scale-down' },
  { doc: 'user-guide/gang-scheduling' },
  { doc: 'user-guide/network-topology' },
  { doc: 'user-guide/router-routing' },
  { doc: 'user-guide/config-router' },
  { doc: 'user-guide/kvcache-aware' },
  { doc: 'user-guide/fairness-scheduling' },
  { doc: 'user-guide/session-boost' },
  { doc: 'user-guide/rate-limit' },
  { doc: 'user-guide/gateway-api-support' },
  { doc: 'user-guide/gateway-inference-extension-support' },
  { doc: 'user-guide/router-observability' },
  {
    doc: 'user-guide/prefill-decode-disaggregation/prefill-decode-disaggregation',
    slug: 'user-guide/prefill-decode-disaggregation',
    extension: '.mdx',
  },
  {
    doc: 'user-guide/prefill-decode-disaggregation/sglang-pd-disaggregation',
  },
  {
    doc: 'user-guide/prefill-decode-disaggregation/vllm-ascend-mooncake',
  },
  { doc: 'timeline/roadmap' },
];

for (const version of ['', 'next/']) {
  const docs = [...fallbackDocs];
  if (version === '')
    docs.push({
      doc: 'user-guide/prefill-decode-disaggregation/vllm-pd-disaggregation',
    });
  if (version === 'next/')
    docs.push(
      { doc: 'getting-started/gpu-free-quick-start' },
      { doc: 'user-guide/external-model-provider' },
      {
        doc: 'user-guide/prefill-decode-disaggregation/modelserving-vllm-pd-disaggregation',
      },
    );
  for (const { doc, slug = doc, extension = '.md' } of docs) {
    const route = `/docs/${version}${slug}`;
    test(`fallback document preserves navigation and section links: ${route}`, () => {
      const en = page(route);
      const zh = page(`/zh-Hans${route}`);
      assert.ok(hasLink(en, `/zh-Hans${route}`, 'zh-Hans'));
      assert.ok(hasLink(zh, route, 'en'));
      assert.match(zh, /此页面尚未翻译/);
      assert.ok(tags(zh, 'div').some((tag) => attribute(tag, 'lang') === 'en'));
      assert.doesNotMatch(en, /此页面尚未翻译|Translation unavailable/);
      if (doc !== 'faq') assert.match(zh, /用户指南/);

      const headings = (html) =>
        tags(html, 'h[2-6]').map((tag) => attribute(tag, 'id'));
      assert.deepEqual(
        headings(zh),
        headings(en),
        'section IDs must survive a language switch',
      );

      const links = tags(zh, 'link');
      assert.ok(
        links.some(
          (tag) =>
            attribute(tag, 'rel') === 'canonical' &&
            attribute(tag, 'href') ===
              `https://kthena.volcano.sh/zh-Hans${route}`,
        ),
      );
      assert.ok(
        links.some(
          (tag) =>
            attribute(tag, 'hreflang') === 'en' &&
            attribute(tag, 'href') === `https://kthena.volcano.sh${route}`,
        ),
      );
      const sourceDir = version
        ? 'docs'
        : `versioned_docs/version-${defaultVersion}`;
      assert.ok(
        hasLink(
          zh,
          `https://github.com/volcano-sh/kthena/tree/main/docs/kthena/${sourceDir}/${doc}${extension}`,
        ),
        'edit links must point to the English source',
      );

      for (const image of tags(zh, 'img')) {
        const src = attribute(image, 'src');
        if (src?.startsWith('/')) {
          assert.ok(
            existsSync(new URL(`../build${src}`, import.meta.url)),
            `missing image ${src}`,
          );
        }
      }
    });
  }
}

test('links between fallback documents stay in the selected locale', () => {
  for (const version of ['', 'next/']) {
    const base = `/zh-Hans/docs/${version}`;
    assert.ok(
      hasLink(
        page(`${base}getting-started/installation`),
        `${base}general/cert-manager`,
      ),
    );
    assert.ok(
      hasLink(
        page(`${base}user-guide/gateway-api-support`),
        `${base}getting-started/installation`,
      ),
    );
    assert.ok(
      hasLink(
        page(`${base}reference/kthena-cli`),
        `${base}getting-started/installation#kthena-cli`,
      ),
    );
  }
});

test('untranslated docs and older versions keep their English content with a notice', () => {
  for (const route of [
    '/docs/faq',
    '/docs/general/faq',
    '/docs/next/general/faq',
    '/docs/developer-guide/ci',
    '/docs/next/developer-guide/ci',
    '/docs/timeline/releases',
    '/docs/next/timeline/releases',
    '/docs/architecture/model-booster-controller',
    '/docs/next/architecture/model-booster-controller',
    '/docs/v0.4.0/getting-started/installation',
  ]) {
    const en = page(route);
    const zh = page(`/zh-Hans${route}`);
    assert.doesNotMatch(en, /此页面尚未翻译|Translation unavailable/);
    assert.match(zh, /此页面尚未翻译/);
    assert.ok(tags(zh, 'div').some((tag) => attribute(tag, 'lang') === 'en'));
    assert.ok(hasLink(zh, route, 'en'));
  }
});

const fallbackPosts = [
  {
    slug: 'scoreplugin-benchmark-blog-post',
    source: '2025-09-09-benchmark/index.md',
  },
  { slug: 'gateway-api-support', source: 'gateway-api-support/index.md' },
  {
    slug: 'launch-blog-post',
    source: 'launch/kthena_llm_inference.mdx',
  },
  { slug: 'modelserving-blog-post', source: 'modelserving/index.md' },
  { slug: 'release-v0.3.0', source: 'release-v0.3.0/index.md' },
  { slug: 'release-v0.4.0', source: 'release-v0.4.0/index.md' },
  {
    slug: 'release-v1.0.0',
    source: 'release-v1.0.0/index.md',
  },
  { slug: 'router-blog-post', source: 'router/index.md' },
];

test('the Chinese blog index lists every English fallback post', () => {
  const index = page('/zh-Hans/blog');
  assert.match(index, /此页面尚未翻译/);
  for (const { slug } of fallbackPosts) {
    assert.ok(hasLink(index, `/zh-Hans/blog/${slug}`));
  }
});

for (const { slug, source } of fallbackPosts) {
  const route = `/blog/${slug}`;
  test(`fallback blog post preserves navigation and section links: ${route}`, () => {
    const en = page(route);
    const zh = page(`/zh-Hans${route}`);
    assert.ok(hasLink(en, `/zh-Hans${route}`, 'zh-Hans'));
    assert.ok(hasLink(zh, route, 'en'));
    assert.match(zh, /此页面尚未翻译/);
    assert.ok(tags(zh, 'div').some((tag) => attribute(tag, 'lang') === 'en'));
    assert.doesNotMatch(en, /此页面尚未翻译|Translation unavailable/);

    const headings = (html) =>
      tags(html, 'h[2-6]').map((tag) => attribute(tag, 'id'));
    assert.deepEqual(
      headings(zh),
      headings(en),
      'section IDs must survive a language switch',
    );

    const links = tags(zh, 'link');
    assert.ok(
      links.some(
        (tag) =>
          attribute(tag, 'rel') === 'canonical' &&
          attribute(tag, 'href') ===
            `https://kthena.volcano.sh/zh-Hans${route}`,
      ),
    );
    assert.ok(
      links.some(
        (tag) =>
          attribute(tag, 'hreflang') === 'en' &&
          attribute(tag, 'href') === `https://kthena.volcano.sh${route}`,
      ),
    );
    assert.ok(
      hasLink(
        zh,
        `https://github.com/volcano-sh/kthena/tree/main/docs/kthena/blog/${source}`,
      ),
      'edit link must point to the English source',
    );

    for (const image of tags(zh, 'img')) {
      const src = attribute(image, 'src');
      if (src?.startsWith('/')) {
        assert.ok(
          existsSync(new URL(`../build${src}`, import.meta.url)),
          `missing image ${src}`,
        );
      }
    }
  });
}
