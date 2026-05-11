# kthena - kthena CLI

`kthena` is a command-line interface tool for managing kthena AI inference workloads in Kubernetes clusters.

## Architecture Diagrams

### Use Case Diagram

```plantuml

actor "User" as user

package "kthena CLI" {
  
  package "Verb Layer" as verbLayer{
    usecase "Get" as GetVerb
    usecase "Describe" as DescribeVerb
    usecase "Create" as CreateVerb
  }
  
  package "Resource Layer" as ResourceLayer {
    usecase "Template(s)" as Template
    usecase "Manifest" as Manifest
    
    rectangle "Kubernetes Resources" as KubernetesResources {
       usecase "Model(s)" as ModelResource
       usecase "ModelInfer(s)" as ModelInferResource
       usecase "Policy(s)" as PolicyResource
       usecase "PolicyBinding(s)" as PolicyBindingResource
    }
  }
  
  package "Flag Layer" as FlagLayer{
    usecase "--dry-run" as DryRunFlag
    usecase "--namespace" as NamespaceFlag
  }
}

' User interactions with verb layer (kubectl-style)
user --> GetVerb
user --> DescribeVerb
user --> CreateVerb

' Verb layer connects to resource layer
CreateVerb --> Manifest
GetVerb --> Template
GetVerb --> KubernetesResources

DescribeVerb --> Template
DescribeVerb --> KubernetesResources

' Resources can use flags
Manifest --> NamespaceFlag
Manifest --> DryRunFlag

note top of CreateVerb
  Create Kthena resources from
  templates with custom values
end note

' layout
GetVerb -[hidden]-> ModelResource
GetVerb -[hidden]-> ModelInferResource
GetVerb -[hidden]-> PolicyResource
GetVerb -[hidden]-> PolicyBindingResource

verbLayer -[hidden]-> ResourceLayer
ResourceLayer -[hidden]-> FlagLayer
```

## Overview

The `kthena` CLI follows kubectl-style verb-noun grammar and provides an easy way to:
- Get and view templates and Kthena resources in your cluster
- Describe detailed information about specific templates and resources
- Create Kthena resources from predefined manifest templates
- Manage inference workloads, models, and autoscaling policies with kubectl-like commands

### Install

```bash
go install github.com/volcano-sh/kthena/cli/kthena@latest
```

### Build from Source

```bash
# From the project root directory
go build -o bin/kthena cli/kthena/main.go
```

### Add to PATH (Optional)

```bash
# Add the bin directory to your PATH
export PATH=$PATH:$(pwd)/bin
```

## Usage

The `kthena` CLI follows kubectl-style verb-noun grammar for consistency and ease of use.

### Template Operations

List all available templates:
```bash
kthena get templates
```

Get a specific template content:
```bash
kthena get template deepseek-r1-distill-llama-8b
kthena get template deepseek-r1-distill-llama-8b -o yaml
```

Describe a template with detailed information:
```bash
kthena describe template DeepSeek-R1-Distill-Qwen-32B
```

### Resource Operations

List models:
```bash
kthena get model-boosters
kthena get model-boosters --all-namespaces
kthena get model-boosters -n production
```

List model serving workloads:
```bash
kthena get model-servings
kthena get model-servings --all-namespaces
```

List autoscaling policies:
```bash
kthena get autoscaling-policies
kthena get autoscaling-policies -n production
```

### Creating Resources

Create resources from templates:
```bash
kthena create manifest --template deepseek-r1-distill-llama-8b --name my-model
kthena create manifest --template deepseek-r1-distill-llama-8b --values-file values.yaml
kthena create manifest --template deepseek-r1-distill-llama-8b --name my-model --dry-run
```

For more detailed usage information, run:
```bash
kthena --help
kthena get --help
kthena describe --help
kthena create --help
```

## Configuration

The CLI uses your local kubectl configuration. Ensure you have:
- Valid kubeconfig file (usually `~/.kube/config`)
- Access to a Kubernetes cluster with Kthena CRDs installed
- Appropriate RBAC permissions for the target namespaces

## Contributing

To add new manifest templates:

1. Create a new `.yaml` file in the `templates/` directory
2. Use Go template syntax with variables: `{{.variable_name}}`
3. Add a description comment at the top: `# Description: Your template description`
4. Test with `kthena describe template your-template`

Example template structure:
```yaml
# Description: Your template description
# Variables: var1, var2, var3
---
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: ModelInfer
metadata:
  name: {{.name}}
  namespace: {{.namespace}}
spec:
  # ... template content
```

## About Cobra

Kthena CLI is built with [Cobra](https://github.com/spf13/cobra).
The building blocks of cobra are depicted as below.

```plantuml
!theme plain
skinparam linetype ortho
skinparam nodesep 10
skinparam ranksep 20

' Main tree structure
rectangle "**Cobra CLI Application**\n//Entry Point//" as App #lightblue

rectangle "**Root Command**\n//cobra.Command//\n- Entry point\n- Execute()\n- Global configuration" as Root #lightgreen

rectangle "**Sub Command 1**\n//cobra.Command//\n- Specific functionality\n- Inherits from parent" as Sub1 #lightgreen

rectangle "**Sub Command 2**\n//cobra.Command//\n- Different functionality\n- Can have own subcommands" as Sub2 #lightgreen

rectangle "**Sub-Sub Command**\n//cobra.Command//\n- Nested functionality\n- Deep command hierarchy" as SubSub #lightgreen

' Flags branch
rectangle "**Flags**\n//Command Options//" as FlagsRoot #lightyellow
rectangle "**Persistent Flags**\n- Available to all subcommands\n- Inherited down the tree\n- Global configuration" as PFlags #lightyellow
rectangle "**Local Flags**\n- Command-specific only\n- Not inherited\n- Local configuration" as LFlags #lightyellow

' Arguments branch
rectangle "**Arguments**\n//Positional Parameters//" as ArgsRoot #lightcyan
rectangle "**Validation Rules**\n- NoArgs\n- MinimumNArgs(n)\n- ExactArgs(n)\n- Custom validation" as ArgValidation #lightcyan
rectangle "**Argument Values**\n- []string args\n- Parsed by cobra\n- Passed to Run function" as ArgValues #lightcyan

' Hooks/Lifecycle branch
rectangle "**Lifecycle Hooks**\n//Execution Flow//" as Hooks #lightpink
rectangle "**Pre-Execution**\n- PersistentPreRun\n- PreRun\n- Setup and validation" as PreHooks #lightpink
rectangle "**Execution**\n- Run function\n- Main command logic\n- Core functionality" as RunHook #lightpink
rectangle "**Post-Execution**\n- PostRun\n- PersistentPostRun\n- Cleanup and finalization" as PostHooks #lightpink

' External Integration branch
rectangle "**External Integration**\n//Third-party Libraries//" as External #lavender
rectangle "**Viper**\n- Configuration management\n- File/env/flag binding\n- Hot reloading" as Viper #lavender
rectangle "**Pflag**\n- POSIX-compliant flags\n- Flag parsing engine\n- Type conversion" as Pflag #lavender

' Features branch
rectangle "**Features**\n//Built-in Capabilities//" as Features #wheat
rectangle "**Auto Help**\n- Generated help text\n- Usage information\n- Command discovery" as Help #wheat
rectangle "**Shell Completion**\n- Bash completion\n- Zsh completion\n- PowerShell completion" as Completion #wheat
rectangle "**Error Handling**\n- Command validation\n- Flag validation\n- Custom error messages" as ErrorHandling #wheat

' Main tree connections
App ||--|| Root : "starts with"
Root ||--o{ Sub1 : "contains"
Root ||--o{ Sub2 : "contains"
Sub1 ||--o{ SubSub : "can contain"

' Flags connections
Root ||--|| FlagsRoot : "has"
FlagsRoot ||--|| PFlags : "includes"
FlagsRoot ||--|| LFlags : "includes"

' Arguments connections
Root ||--|| ArgsRoot : "validates"
ArgsRoot ||--|| ArgValidation : "applies"
ArgsRoot ||--|| ArgValues : "processes"

' Hooks connections
Root ||--|| Hooks : "executes"
Hooks ||--|| PreHooks : "runs first"
Hooks ||--|| RunHook : "runs main"
Hooks ||--|| PostHooks : "runs last"

' External connections
Root ||..|| External : "integrates"
External ||--|| Viper : "uses"
External ||--|| Pflag : "built on"

' Features connections
Root ||--|| Features : "provides"
Features ||--|| Help : "generates"
Features ||--|| Completion : "supports"
Features ||--|| ErrorHandling : "handles"

' Inheritance flows (dotted lines for inheritance)
Sub1 -.-> Root : "inherits flags & hooks"
Sub2 -.-> Root : "inherits flags & hooks"
SubSub -.-> Sub1 : "inherits flags & hooks"

note top of App
**Execution Flow:**
main() → rootCmd.Execute() → 
flag parsing → argument validation → 
hook execution → command logic
end note

note bottom of PFlags
**Flag Inheritance:**
Persistent flags flow down
the command tree to all
child commands
end note

note right of Hooks
**Hook Execution Order:**
1. PersistentPreRun (parent)
2. PreRun (current command)
3. Run (main logic)
4. PostRun (current command)
5. PersistentPostRun (parent)
end note

```
