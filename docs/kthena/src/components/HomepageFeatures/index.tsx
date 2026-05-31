import type { ReactNode } from 'react';
import clsx from 'clsx';
import Heading from '@theme/Heading';
import styles from './styles.module.css';

type FeatureItem = {
  title: string;
  Svg: React.ComponentType<React.ComponentProps<'svg'>>;
  description: ReactNode;
};

const FeatureList: FeatureItem[] = [
  {
    title: 'Kubernetes Native',
    Svg: require('@site/static/img/homepage/kthena-feature-1.svg').default,
    description: (
      <>
        Declarative CRDs for end-to-end AI inference lifecycle management.
        Gang scheduling, network-topology-aware placement, and Volcano
        integration bring enterprise-grade orchestration to your existing
        K8s infrastructure.
      </>
    ),
  },
  {
    title: 'Intelligent Routing',
    Svg: require('@site/static/img/homepage/kthena-feature-2.svg').default,
    description: (
      <>
        Request-level scheduling with pluggable scoring plugins—least latency,
        KV-cache awareness, prefix-cache matching, and LoRA affinity routing.
        Per-model fair queuing and token-based rate limiting ensure optimal
        throughput for every model.
      </>
    ),
  },
  {
    title: 'Hierarchical PD Disaggregation Orchestration',
    Svg: require('@site/static/img/homepage/kthena-feature-3.svg').default,
    description: (
      <>
        Separate prefill and decode phases into independently scalable serving
        groups. Prefill nodes maximize compute throughput while decode nodes
        optimize for low latency, enabling fine-grained GPU utilization and
        flexible scaling ratios.
      </>
    ),
  },
];

function Feature({ title, Svg, description }: FeatureItem) {
  return (
    <div className={clsx('col col--4')}>
      <div className="text--center">
        <Svg className={styles.featureSvg} role="img" />
      </div>
      <div className="text--center padding-horiz--md">
        <Heading as="h3">{title}</Heading>
        <p>{description}</p>
      </div>
    </div>
  );
}

export default function HomepageFeatures(): ReactNode {
  return (
    <section className={styles.features}>
      <div className="container">
        <div className="row">
          {FeatureList.map((props, idx) => (
            <Feature key={idx} {...props} />
          ))}
        </div>
      </div>
    </section>
  );
}
