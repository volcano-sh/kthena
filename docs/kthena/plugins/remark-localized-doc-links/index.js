const path = require('node:path');

const siteDir = path.resolve(__dirname, '../..');

/**
 * Docusaurus 3.9 resolves ./ and ../ links only from the physical source file.
 * With partial translations, the target can live in the other content tree.
 * Content-root-relative Markdown links let its resolver search both trees and
 * still resolve slugs, versions, locales, and broken links normally.
 */
module.exports = function remarkLocalizedDocLinks() {
  return (tree, file) => {
    const source = path.relative(siteDir, file.path).split(path.sep).join('/');
    const match = source.match(
      /^(?:docs\/|versioned_docs\/version-[^/]+\/|i18n\/[^/]+\/docusaurus-plugin-content-docs\/(?:current|version-[^/]+)\/)(.+)$/,
    );
    if (!match) {
      return;
    }
    const sourceDir = path.posix.dirname(match[1]);

    function visit(node) {
      if (node.type === 'link' || node.type === 'definition') {
        const link = node.url?.match(/^(\.\.?\/[^?#]*\.mdx?)([?#].*)?$/);
        if (link) {
          const target = path.posix.normalize(
            path.posix.join(sourceDir, link[1]),
          );
          // Leave links outside the documentation tree to the normal resolver.
          if (!target.startsWith('../')) {
            node.url = `/${target}${link[2] ?? ''}`;
          }
        }
      }
      node.children?.forEach(visit);
    }

    visit(tree);
  };
};
