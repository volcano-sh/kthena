import type { ReactNode } from 'react';
import { useDoc } from '@docusaurus/plugin-content-docs/client';
import Content from '@theme-original/DocItem/Content';
import type { Props } from '@theme/DocItem/Content';
import TranslationFallback from '@site/src/components/TranslationFallback';

export default function DocItemContent(props: Props): ReactNode {
  const { metadata } = useDoc();
  return (
    <TranslationFallback source={metadata.source}>
      <Content {...props} />
    </TranslationFallback>
  );
}
