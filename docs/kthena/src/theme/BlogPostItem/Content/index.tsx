import type { ReactNode } from 'react';
import { useBlogPost } from '@docusaurus/plugin-content-blog/client';
import Content from '@theme-original/BlogPostItem/Content';
import type { Props } from '@theme/BlogPostItem/Content';
import TranslationFallback from '@site/src/components/TranslationFallback';

export default function BlogPostItemContent(props: Props): ReactNode {
  const { metadata } = useBlogPost();
  return (
    <TranslationFallback source={metadata.source}>
      <Content {...props} />
    </TranslationFallback>
  );
}
