# Kthena CLI

The Kthena CLI provides kubectl‑style commands for managing AI inference workloads on Kubernetes, featuring quick deployment via curated templates and optional integration with kubectl‑ai for natural‑language command generation.

This document is divided into two parts:

1. **[Using the Kthena CLI](#using-the-kthena-cli)** – How to install and use the CLI directly.
2. **[Integrating with kubectl‑ai](#integrating-with-kubectlai)** – How to enable AI‑powered command generation with kubectl‑ai.

## Using the Kthena CLI

### Overview

The Kthena CLI offers a set of subcommands for managing AI inference workloads:

- **`kthena get`** – Display one or many resources (templates, model‑servings, autoscaling policies, etc.)
- **`kthena create`** – Create resources from predefined manifests and templates
- **`kthena describe`** – Show detailed information about a specific resource

Each subcommand supports additional resource‑specific operations. For a complete reference, see the dedicated documentation:

- [kthena](kthena-cli/kthena.md) – General CLI overview and global options
- [kthena get](kthena-cli/kthena_get.md) – Display resources
- [kthena create](kthena-cli/kthena_create.md) – Create resources
- [kthena describe](kthena-cli/kthena_describe.md) – Describe resources

### Installation

To install the Kthena CLI, see the [Installation Guide](../getting-started/installation.md#kthena-cli).

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
   See the [Installation Guide](../getting-started/installation.md#kthena-cli).

3. **Place the Kthena tool configuration**  
    By default, kubectl‑ai looks for tool configurations in `~/.config/kubectl‑ai/tools.yaml`. Copy the following tools.yaml content to that location:

    <details>
    <summary>
    <b>tools.yaml</b>
    </summary>
    
    ```yaml
    - name: kthena
      description: "A CLI tool for managing Kthena AI inference workloads in Kubernetes clusters. Use it to create, manage, and monitor AI model deployments."
      command: "kthena"
      command_desc: |
        The kthena command-line interface for AI inference workload management.
    
        For detailed documentation and advanced usage examples, visit https://kthena.volcano.sh/
      
        Core subcommands and usage patterns:
      
        ## Template Management
        - `kthena get templates`: List all available model deployment templates
        - `kthena describe template <template-name>`: Show detailed template content and parameters
        - `kthena get template <template-name> -o yaml`: Get template in YAML format
      
        ## Creating Model Deployments
        - `kthena create manifest --template <template> --name <name>`: Create and deploy a model from template
        - `kthena create manifest --template <template> --name <name> --dry-run`: Preview template rendering without applying
        - `kthena create manifest --template <template> --values-file values.yaml`: Create with custom values from file
        - `kthena create manifest --template <template> --name <name> --set key1=value1,key2=value2`: Set template values directly
      
        ## Resource Management
        - `kthena get model-boosters`: List registered models (requires Kubernetes connection)
        - `kthena get model-servings`: List model serving workloads (requires Kubernetes connection)
        - `kthena get autoscaling-policies`: List autoscaling policies
        - `kthena describe model-booster <name>`: Show detailed model information
        - `kthena describe model-serving <name>`: Show detailed serving workload information
      
        ## Common Templates
        Available templates include:
        - deepseek-ai/DeepSeek-R1-Distill-Qwen-7B
        - deepseek-ai/DeepSeek-R1-Distill-Qwen-32B
        - Qwen/Qwen3-8B
        - Qwen/Qwen3-32B
      
        ## Key Parameters for create manifest
        - `--template`: Template name (required)
        - `--name`: Name for the inference workload
        - `--namespace`: Kubernetes namespace (default: default)
        - `--dry-run`: Preview without applying
        - `--set`: Set template values (key=value pairs)
        - `--values-file`: YAML file with template values
      
        Example workflow:
        1. `kthena get templates` - Browse available models
        2. `kthena describe template deepseek-ai/DeepSeek-R1-Distill-Qwen-32B` - View template details
        3. `kthena create manifest --template deepseek-ai/DeepSeek-R1-Distill-Qwen-32B --name my-deepseek --dry-run` - Preview
        4. `kthena create manifest --template deepseek-ai/DeepSeek-R1-Distill-Qwen-32B --name my-deepseek` - Deploy
    ```
    </details>

4. **Start using kubectl‑ai with Kthena**  
   Run kubectl‑ai with a prompt that references Kthena operations:
   ```bash
   kubectl-ai "List all available model templates using kthena"
   ```

### Tool Configuration Explained

The `tools.yaml` file defines how kubectl‑ai invokes the `kthena` command. It includes:

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
kubectl-ai --custom-tools-config=tools.yaml "your prompt"
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

- [Kthena CLI Documentation](kthena-cli/kthena) – Complete reference for `kthena` commands and flags.
- [kubectl‑ai GitHub Repository](https://github.com/GoogleCloudPlatform/kubectl‑ai) – Official source, installation, and advanced usage.
- [Kthena Project](https://github.com/volcano-sh/kthena) – Main repository with architecture, examples, and contributing guidelines.

---

**Pro Tip**: kubectl‑ai’s effectiveness depends on the clarity of your prompt. Be specific about the operation, resource names, and flags you want to use. The more precise the prompt, the more accurate the generated command.
