import type { ReactNode } from 'react';
import useDocusaurusContext from '@docusaurus/useDocusaurusContext';
import Translate, { translate } from '@docusaurus/Translate';
import Admonition from '@theme/Admonition';

type Props = {
  source: string;
  children: ReactNode;
};

export default function TranslationFallback({
  source,
  children,
}: Props): ReactNode {
  const { i18n, siteConfig } = useDocusaurusContext();
  const { currentLocale, defaultLocale, localeConfigs } = i18n;
  // Content metadata identifies the actual source selected by Docusaurus,
  // including when it falls back to an untranslated document or blog post.
  const translatedSource = `@site/${siteConfig.i18n.path}/${localeConfigs[currentLocale].path}/`;
  const isFallback =
    currentLocale !== defaultLocale && !source.startsWith(translatedSource);

  if (!isFallback) {
    return children;
  }

  return (
    <>
      <Admonition
        type="info"
        title={translate({
          id: 'translationFallback.title',
          message: 'Translation unavailable',
        })}
      >
        <Translate id="translationFallback.message">
          This page has not been translated yet. The original English content is
          shown below.
        </Translate>
      </Admonition>
      <div lang={localeConfigs[defaultLocale].htmlLang}>{children}</div>
    </>
  );
}
