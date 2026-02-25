import type { SidebarsConfig } from '@docusaurus/plugin-content-docs';

// This runs in Node.js - Don't use client-side code here (browser APIs, JSX...)

/**
 * Creating a sidebar enables you to:
 - create an ordered group of docs
 - render a sidebar for each doc of that group
 - provide next/previous navigation

 The sidebars can be generated from the filesystem, or explicitly defined here.

 Create as many sidebars as you want.
 */
const sidebars: SidebarsConfig = {
  // Kthena documentation sidebar
  tutorialSidebar: [
    'intro',
    {
      type: 'category',
      label: 'Getting Started',
      items: ['getting-started/installation', 'getting-started/quick-start'],
    },
    {
      type: 'category',
      label: 'Architecture',
      link: {
        type: 'doc',
        id: 'architecture/architecture',
      },
      items: [
        'architecture/model-booster-controller',
        'architecture/model-serving-controller',
        'architecture/kthena-router',
        'architecture/autoscaler',
      ],
    },
    {
      type: 'category',
      label: 'User Guide',
      link: {
        type: 'generated-index',
        title: 'User Guide Overview',
        description: 'A comprehensive overview of all the user guides.',
      },
      items: [
        {
          type: 'category',
          label: 'Model Deployment',
          link: {
            type: 'doc',
            id: 'user-guide/model-deployment',
          },
          items: [
            'user-guide/lws-integration',
          ],
        },
        'user-guide/multi-node-inference',
        'user-guide/network-topology',
        {
          type: 'doc',
          label: 'Autoscaler',
          id: 'user-guide/autoscaler',
        },
        {
          type: 'category',
          label: 'workload',
          items: [
            'user-guide/binpack-scale-down',
            'user-guide/gang-scheduling',
            'user-guide/network-topology',
          ],
        },
        {
          type: 'category',
          label: 'Router',
          items: [
            'user-guide/router-routing',
            'user-guide/config-router',
            'user-guide/rate-limit',
            "user-guide/gateway-api-support",
            'user-guide/gateway-inference-extension-support',
          ],
        },
        {
          type: 'category',
          label: 'Observability',
          items: [
            'user-guide/router-observability',
          ],
        },
        'user-guide/runtime',
        'user-guide/binpack-scale-down',
        {
          type: 'category',
          label: 'Prefill Decode Disaggregation',
          link: {
            type: 'doc',
            id: 'user-guide/prefill-decode-disaggregation/prefill-decode-disaggregation',
          },
          items: [
            'user-guide/prefill-decode-disaggregation/vllm-ascend-mooncake',
          ],
        },
      ],
    },
    {
      type: 'category',
      label: 'General',
      items: ['general/cert-manager', 'general/faq', 'general/prometheus', 'general/data-parallel-deployment'],
    },
    {
      type: 'category',
      label: 'Developer Guide',
      items: [
        'developer-guide/development-setup',
        'developer-guide/release',
        'developer-guide/model-serving-rolling-update',
        'developer-guide/ci',
        'developer-guide/model-serving-scaling',
      ],
    },
    {
      type: 'category',
      label: 'Timeline',
      items: ['timeline/roadmap', 'timeline/releases'],
    },
    {
      type: 'category',
      label: 'Reference',
      items: [
        {
          type: 'doc',
          id: 'reference/helm-chart-values',
          label: 'Helm Chart Values',
        },
        {
          type: 'category',
          label: 'CRD Reference',
          items: [
            {
              type: 'doc',
              id: 'reference/crd/networking.serving.volcano.sh',
              label: 'Networking',
            },
            {
              type: 'doc',
              id: 'reference/crd/workload.serving.volcano.sh',
              label: 'Workload',
            },
          ],
        },
        {
          type: 'category',
          label: 'Kthena CLI',
          link: {
            type: 'doc',
            id: 'reference/kthena-cli',
          },
          items: [
            { type: 'doc', id: 'reference/kthena-cli/kthena', label: 'Kthena' },
            {
              type: 'category',
              label: 'Create',
              link: {
                type: 'doc',
                id: 'reference/kthena-cli/kthena_create',
              },
              items: [
                { type: 'doc', id: 'reference/kthena-cli/kthena_create_manifest', label: 'Create manifest' },
              ],
            },
            {
              type: 'category',
              label: 'Get',
              link: {
                type: 'doc',
                id: 'reference/kthena-cli/kthena_get',
              },
              items: [
                { type: 'doc', id: 'reference/kthena-cli/kthena_get_autoscaling-policies', label: 'Get autoscaling-policies' },
                { type: 'doc', id: 'reference/kthena-cli/kthena_get_autoscaling-policy-bindings', label: 'Get autoscaling-policy-bindings' },
                { type: 'doc', id: 'reference/kthena-cli/kthena_get_model-boosters', label: 'Get model-boosters' },
                { type: 'doc', id: 'reference/kthena-cli/kthena_get_model-servings', label: 'Get model-servings' },
                { type: 'doc', id: 'reference/kthena-cli/kthena_get_template', label: 'Get template' },
                { type: 'doc', id: 'reference/kthena-cli/kthena_get_templates', label: 'Get templates' },
              ],
            },
            {
              type: 'category',
              label: 'Describe',
              link: {
                type: 'doc',
                id: 'reference/kthena-cli/kthena_describe',
              },
              items: [
                { type: 'doc', id: 'reference/kthena-cli/kthena_describe_autoscaling-policy', label: 'Describe autoscaling-policy' },
                { type: 'doc', id: 'reference/kthena-cli/kthena_describe_model-booster', label: 'Describe model-booster' },
                { type: 'doc', id: 'reference/kthena-cli/kthena_describe_model-serving', label: 'Describe model-serving' },
                { type: 'doc', id: 'reference/kthena-cli/kthena_describe_template', label: 'Describe template' },
              ],
            },
          ],
        },
        {
          type: 'doc',
          id: 'reference/router-access-log-fields',
          label: 'Router Access Log Fields',
        },
      ],
    },
  ],
};

export default sidebars;
