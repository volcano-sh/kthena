# Helm Templates for Kthena CLI

This directory contains Helm templates for the Kthena CLI, designed to provide a set of curated, production‑ready manifests for common AI inference workloads.

## Design Principles

Templates in this repository adhere to the following principles:

- **Frequently Used**: Each template addresses a common deployment pattern observed in real‑world usage, ensuring practical relevance.
- **Quick‑Start**: Templates are minimal and self‑contained, enabling users to deploy a working configuration with minimal customization.
- **Simple**: Complexity is deliberately kept low to reduce the cognitive load and ease debugging. Each template focuses on a single, clear use case.
- **Tested**: Every template is validated against a live Kubernetes cluster with the Kthena CRDs installed, guaranteeing functional correctness.
- **State‑of‑the‑Art (SOTA)**: Templates are regularly updated to reflect the latest best practices, model architectures, and Kubernetes API conventions.

## Scope and Limitations

Sophisticated or highly customized use cases are intentionally omitted from the CLI template library for four primary reasons:

1. **Combinatorial Explosion**: The number of possible variations (model types, resource settings, scaling policies, networking options, etc.) grows exponentially, making it impractical to maintain a comprehensive set of static examples.
2. **Rapid Evolution**: The landscape of state‑of‑the‑art models and inference frameworks changes quickly. Keeping a large catalog of complex templates up‑to‑date would require disproportionate maintenance effort.
3. **Terminal UX Limitations**: Complex templates are difficult to view, navigate, and customize effectively within a terminal environment. Users are better served by using dedicated IDEs or editors that provide syntax highlighting, validation, and advanced editing capabilities.
4. **CRD Instability**: The underlying Custom Resource Definitions (CRDs) for Kthena may evolve over time, leading to breaking changes. Maintaining a large set of complex templates would necessitate continuous, effort-intensive updates to keep them compatible with the latest CRD versions.

## Where to Find Advanced Examples

For advanced configuration, customizations, and edge‑case examples:

- Consult the official [Kthena documentation](https://kthena.volcano.sh) for detailed guides and reference architectures.
- Use AI‑assisted tools such as `kubectl‑ai` or the built‑in “ask‑ai” search bar on the Kthena website to generate tailored YAML manifests for your specific requirements.

## Contributing a New Template

If you wish to contribute a new template, please ensure it aligns with the design principles above and includes:

1. A clear, concise description at the top of the file.
2. A minimal set of customizable parameters using Go template syntax (`{{.variable}}`).
3. A corresponding test that validates the template against a real cluster.

Submit your contribution via a pull request, and the maintainers will review it for inclusion.
