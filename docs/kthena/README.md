## Contributing to Documentation

This section provides guidelines for developers who want to contribute to the Kthena documentation.

### Adding New Documentation

1. **Create a new markdown file** in the appropriate directory under `docs/`
2. **Update the sidebar** by editing `sidebars.ts` to include your new page in the navigation
3. **Follow the naming convention**: Use lowercase with hyphens (e.g., `my-new-feature.md`)

### Writing Guidelines

- Use clear, concise language
- Include code examples where applicable
- Add proper headings hierarchy (H1 for page title, H2 for main sections, etc.)
- Use markdown formatting consistently
- Include links to related documentation when relevant

### Testing Your Changes

1. **Start the development server**:
   ```bash
   npm run start
   ```
2. **Preview your changes** in the browser at `http://localhost:3000`
3. **Check for broken links** and ensure navigation works correctly
4. **Build the site** to verify no build errors:
   ```bash
   npm run build
   ```

### Sidebar Configuration

To add your new documentation to the sidebar navigation, edit `sidebars.ts`:

```typescript
const sidebars: SidebarsConfig = {
  tutorialSidebar: [
    'intro',
    {
      type: 'category',
      label: 'Your Category',
      items: [
        'your-category/your-new-page',
      ],
    },
  ],
};
```

### Contribution Workflow

1. Create a new branch for your documentation changes
2. Add or modify documentation files
3. Update `sidebars.ts` if adding new pages
4. Test locally using `npm run start`
5. Build the site to ensure no errors: `npm run build`
6. Submit a pull request with a clear description of your changes


---
## Documentation Generation with crd-ref-docs

This document explains how to use `crd-ref-docs` to generate API reference documentation for the Kthena project's Custom Resource Definitions (CRDs).

### Overview

The Kthena project uses [crd-ref-docs](https://github.com/elastic/crd-ref-docs) to automatically generate comprehensive API reference documentation from the Go source code of our Custom Resource Definitions. This tool creates markdown documentation that includes:

- Detailed field descriptions for all CRD types
- Validation rules and constraints
- Cross-references between related types
- Kubernetes API compatibility information

### Prerequisites

- Go 1.19 or later
- Make utility
- Access to the Kthena project repository

### Quick Start

To generate the API documentation, simply run:

```bash
make gen-docs
```

This command will:
1. Download and install the `crd-ref-docs` tool (if not already present)
2. Scan the `pkg/apis` directory for CRD definitions
3. Generate markdown documentation in `docs/kthena/docs/api/`

### Configuration

The documentation generation is configured via `docs/kthena/crd-ref-docs-config.yaml`:

```yaml
# Minimal configuration for crd-ref-docs
processor:
  ignoreFields: []
render:
  kubernetesVersion: "1.33"
```

#### Configuration Options

- **processor.ignoreFields**: List of field names to exclude from documentation
- **render.kubernetesVersion**: Target Kubernetes version for API compatibility

For advanced configuration options, refer to the [crd-ref-docs documentation](https://github.com/elastic/crd-ref-docs#configuration).

#### API Groups

The Kthena project defines CRDs in three main API groups:

- **registry.volcano.sh**: Model registry and autoscaling policies
- **networking.volcano.sh**: Network routing and traffic management
- **workload.volcano.sh**: Workload management and scheduling

### Customizing Documentation

#### Adding Field Descriptions

To improve the generated documentation, add Go comments to your struct fields:

```go
type ModelSpec struct {
    // Name is the human-readable name of the model
    // +kubebuilder:validation:Required
    Name string `json:"name"`
    
    // Version specifies the model version to deploy
    // +kubebuilder:validation:Pattern=^v\d+\.\d+\.\d+$
    Version string `json:"version"`
}
```

#### Ignoring Fields

To exclude sensitive or internal fields from documentation, add them to the configuration:

```yaml
processor:
  ignoreFields:
    - "internalField"
    - "secretData"
```

### Troubleshooting

#### Common Issues

1. **"No CRDs found"**: Ensure your CRD types are properly tagged with `+kubebuilder:object:root=true`

2. **"Max recursion depth reached"**: This warning indicates circular references in your types. Consider simplifying complex nested structures.

3. **Missing field descriptions**: Add Go comments above struct fields to provide documentation.

#### Debugging

To see detailed output during generation:

```bash
make gen-docs VERBOSE=1
```

### Integration with CI/CD

To ensure documentation stays up-to-date, add the documentation generation to your CI pipeline:

```yaml
- name: Generate API Documentation
  run: make gen-docs
  
- name: Check for documentation changes
  run: git diff --exit-code docs/kthena/docs/api/
```

### Manual Tool Installation

If you need to install `crd-ref-docs` manually:

```bash
# Install specific version
go install github.com/elastic/crd-ref-docs@v0.2.0

# Or use the Makefile target
make crd-ref-docs
```

---
## Deployment

Using SSH:

```bash
USE_SSH=true npm run deploy
```

Not using SSH:

```bash
GIT_USER=<Your GitHub username> npm run deploy
```

If you are using GitHub pages for hosting, this command is a convenient way to build the website and push to the `gh-pages` branch.

## Manage Docs Versions

Release a version 1.0 of your project:

```shell
npm run docusaurus docs:version v0.2.0
```

Please refer to [this guide](https://tutorial.docusaurus.io/docs/tutorial-extras/manage-docs-versions/) for more information.