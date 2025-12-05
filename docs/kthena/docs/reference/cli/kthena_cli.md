# Kthena CLI

The Kthena CLI (`kthena`) is a command-line interface for managing AI inference workloads on Kubernetes. It provides commands to create, inspect, and manage model deployments, templates, autoscaling policies, and other Kthena resources.

This document is divided into two parts:

1. **[Using the Kthena CLI](#using-the-kthena-cli)** – How to install and use the CLI directly.
2. **[Integrating with kubectl‑ai](#integrating-with-kubectl-ai)** – How to enable AI‑powered command generation with kubectl‑ai.

## Using the Kthena CLI

### Overview

The Kthena CLI offers a set of subcommands for managing AI inference workloads:

- **`kthena get`** – Display one or many resources (templates, model‑servings, autoscaling policies, etc.)
- **`kthena create`** – Create resources from predefined manifests and templates
- **`kthena describe`** – Show detailed information about a specific resource

Each subcommand supports additional resource‑specific operations. For a complete reference, see the dedicated documentation:

- [kthena](kthena.md) – General CLI overview and global options
- [kthena get](kthena_get.md) – Display resources
- [kthena create](kthena_create.md) – Create resources
- [kthena describe](kthena_describe.md) – Describe resources

### Installation

Download the latest binary from the [releases page](https://github.com/volcano-sh/kthena/releases) or build from source:

```bash
go build -o bin/kthena cli/kthena/main.go
export PATH=$PATH:$(pwd)/bin
```

### Quick Examples

```bash
# List all available model templates
kthena get templates

# Describe a specific template
kthena describe template deepseek-ai/DeepSeek-R1-Distill-Qwen-32B

# Create a model deployment from a template (dry‑run)
kthena create manifest --template Qwen/Qwen3-8B --name my-qwen --dry-run

# List model serving workloads across all namespaces
kthena get model-servings --all-namespaces
```

For more detailed usage, refer to the subcommand documentation linked above.

## Integrating with kubectl‑ai

### Overview

**[kubectl‑ai](https://github.com/GoogleCloudPlatform/kubectl‑ai)** is an open‑source plugin that uses large language models to interpret natural‑language prompts and generate appropriate Kubernetes commands. It ships with built‑in tools such as `kubectl` and `bash`, but can be extended with custom tools.

By adding Kthena as a custom tool, you enable kubectl‑ai to perform advanced AI‑serving operations—such as deploying DeepSeek or Qwen models, managing scaling policies, and inspecting running workloads—using simple English prompts.

### Prerequisites

- [kubectl‑ai](https://github.com/GoogleCloudPlatform/kubectl‑ai) installed and configured
- [Kthena CLI](https://github.com/volcano-sh/kthena/releases) installed and available in your `PATH`
- A Kubernetes cluster with Kthena CRDs installed (if you intend to apply resources)

### Quick Start

1. **Install kubectl‑ai**  
   Follow the [official installation guide](https://github.com/GoogleCloudPlatform/kubectl‑ai#installation).

2. **Install Kthena CLI**  
   As described in the [Installation](#installation) section above.

3. **Place the Kthena tool configuration**  
   By default, kubectl‑ai looks for tool configurations in `~/.config/kubectl‑ai/tools.yaml`. Copy the provided [`tools.yaml`](../../../../cli/kubectl-ai/tools.yaml) to that location:
   ```bash
   mkdir -p ~/.config/kubectl-ai
   cp cli/kubectl-ai/tools.yaml ~/.config/kubectl-ai/
   ```

4. **Start using kubectl‑ai with Kthena**  
   Run kubectl‑ai with a prompt that references Kthena operations:
   ```bash
   kubectl-ai "List all available model templates using kthena"
   ```

### Tool Configuration Explained

The [`tools.yaml`](../../../../cli/kubectl-ai/tools.yaml) file defines how kubectl‑ai invokes the `kthena` command. It includes:

- **Command description**: A detailed overview of Kthena’s capabilities and subcommands.
- **Common usage patterns**: Examples for template management, creating deployments, and inspecting resources.
- **Available templates**: A list of curated model templates (DeepSeek‑R1, Qwen, etc.).
- **Key parameters**: Flags like `--template`, `--name`, `--dry‑run`, and `--values‑file`.

When kubectl‑ai processes a prompt, it matches the intent to one of the described subcommands and generates the appropriate `kthena` invocation.

### Example Workflows

#### 1. Browse Available Model Templates
```
kubectl-ai "Show me all model templates that kthena can deploy"
```
*Expected output:* `kthena get templates`

#### 2. Deploy a DeepSeek Model
```
kubectl-ai "Create a model deployment named my‑deepseek using the DeepSeek‑R1‑Distill‑Qwen‑32B template in the default namespace"
```
*Expected output:* `kthena create manifest --template deepseek‑ai/DeepSeek‑R1‑Distill‑Qwen‑32B --name my‑deepseek`

#### 3. Preview a Deployment (Dry‑Run)
```
kubectl-ai "Preview the YAML for a Qwen3‑8B deployment without applying it"
```
*Expected output:* `kthena create manifest --template Qwen/Qwen3‑8B --name my‑qwen --dry‑run`

#### 4. Inspect Running Model Servings
```
kubectl-ai "List all model serving workloads in the cluster using kthena"
```
*Expected output:* `kthena get model‑servings --all‑namespaces`

#### 5. Get Detailed Template Information
```
kubectl-ai "Describe the DeepSeek‑R1‑Distill‑Qwen‑7B template"
```
*Expected output:* `kthena describe template deepseek‑ai/DeepSeek‑R1‑Distill‑Qwen‑7B`

### Advanced Configuration

#### Using a Custom Tools Configuration Path
If you prefer not to use the default `~/.config/kubectl‑ai/tools.yaml`, you can specify a custom configuration file or directory with the `--custom‑tools‑config` flag:
```bash
kubectl-ai --custom-tools-config=cli/kubectl-ai/tools.yaml "your prompt"
```

#### Extending the Tools File
You can merge Kthena’s tool definition with other custom tools by editing `tools.yaml`. Refer to the [kubectl‑ai tools documentation](https://github.com/GoogleCloudPlatform/kubectl-ai/blob/main/docs/tools.md) for the full schema.

### Troubleshooting

| Issue | Solution |
|-------|----------|
| kubectl‑ai doesn’t recognize `kthena` commands | Ensure `tools.yaml` is in the correct location and has valid YAML syntax. |
| “Command not found: kthena” | Verify Kthena CLI is installed and available in your `$PATH`. |
| kubectl‑ai suggests incorrect commands | The tool description in `tools.yaml` may need refinement. Adjust the description to better match intended use‑cases. |
| Permission errors when applying resources | Check your kubeconfig context and RBAC permissions for the target namespace. |

## Further Reading

- [Kthena CLI Documentation](https://kthena.volcano.sh/docs/reference/cli/kthena) – Complete reference for `kthena` commands and flags.
- [kubectl‑ai GitHub Repository](https://github.com/GoogleCloudPlatform/kubectl‑ai) – Official source, installation, and advanced usage.
- [Kthena Project](https://github.com/volcano-sh/kthena) – Main repository with architecture, examples, and contributing guidelines.

---

**Pro Tip**: kubectl‑ai’s effectiveness depends on the clarity of your prompt. Be specific about the operation, resource names, and flags you want to use. The more precise the prompt, the more accurate the generated command.
