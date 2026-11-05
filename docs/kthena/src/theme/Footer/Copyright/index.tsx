import type { ReactNode } from 'react';
import { translate } from '@docusaurus/Translate';
import Copyright from '@theme-original/Footer/Copyright';
import type { Props } from '@theme/Footer/Copyright';

export default function FooterCopyright(props: Props): ReactNode {
  return (
    <Copyright
      {...props}
      copyright={translate(
        {
          id: 'footer.copyright',
          message:
            'Copyright © {year} Volcano Community. Built with Docusaurus.',
        },
        { year: new Date().getFullYear() },
      )}
    />
  );
}
