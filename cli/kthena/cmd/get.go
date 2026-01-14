/*
Copyright The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/volcano-sh/kthena/client-go/clientset/versioned"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"
)

var (
	outputFormat     string
	getNamespace     string
	getAllNamespaces bool
)

// getCmd represents the get command
var getCmd = &cobra.Command{
	Use:   "get",
	Short: "Display one or many resources",
	Long: `Display one or many resources.

You can get templates, models, model-servings, and autoscaling policies.

Examples:
  kthena get templates
  kthena get template deepseek-r1-distill-llama-8b
  kthena get template deepseek-r1-distill-llama-8b -o yaml
  kthena get model-boosters
  kthena get model-boosters --all-namespaces
  kthena get model-servings -n production`,
}

// getTemplatesCmd represents the get templates command
var getTemplatesCmd = &cobra.Command{
	Use:   "templates",
	Short: "List available manifest templates",
	Long: `List all available manifest templates that can be used with kthena commands.

Templates are predefined combinations of kthena resources that can be
customized with your specific values.`,
	RunE: runGetTemplates,
}

// getTemplateCmd represents the get template command
var getTemplateCmd = &cobra.Command{
	Use:   "template [NAME]",
	Short: "Get a specific template",
	Long: `Get a specific template by name.

Use -o yaml flag to output the template content in YAML format.`,
	Args: cobra.ExactArgs(1),
	RunE: runGetTemplate,
}

// getModelBoostersCmd represents the get model-boosters command
var getModelBoostersCmd = &cobra.Command{
	Use:     "model-boosters [NAME]",
	Aliases: []string{"model-booster"},
	Short:   "List registered models",
	Long: `List Model resources in the cluster. 

If NAME is provided, only models containing the specified name will be displayed.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runGetModelBoosters,
}

// getModelServingsCmd represents the get model-servings command
var getModelServingsCmd = &cobra.Command{
	Use:     "model-servings",
	Aliases: []string{"ms", "model-serving"},
	Short:   "List model serving workloads",
	Long:    `List ModelServing resources in the cluster.`,
	RunE:    runGetModelServings,
}

// getAutoscalingPoliciesCmd represents the get autoscaling-policies command
var getAutoscalingPoliciesCmd = &cobra.Command{
	Use:     "autoscaling-policies",
	Aliases: []string{"asp", "autoscaling-policy"},
	Short:   "List autoscaling policies",
	Long:    `List AutoscalingPolicy resources in the cluster.`,
	RunE:    runGetAutoscalingPolicies,
}

// getAutoscalingPolicyBindingsCmd represents the get autoscaling-policy-bindings command
var getAutoscalingPolicyBindingsCmd = &cobra.Command{
	Use:     "autoscaling-policy-bindings",
	Aliases: []string{"aspb", "autoscaling-policy-binding"},
	Short:   "List autoscaling policy bindings",
	Long:    `List AutoscalingPolicyBinding resources in the cluster.`,
	RunE:    runGetAutoscalingPolicyBindings,
}

func init() {
	rootCmd.AddCommand(getCmd)
	getCmd.AddCommand(getTemplatesCmd)
	getCmd.AddCommand(getTemplateCmd)
	getCmd.AddCommand(getModelBoostersCmd)
	getCmd.AddCommand(getModelServingsCmd)
	getCmd.AddCommand(getAutoscalingPoliciesCmd)
	getCmd.AddCommand(getAutoscalingPolicyBindingsCmd)

	// Add output format flag
	getCmd.PersistentFlags().StringVarP(&outputFormat, "output", "o", "", "Output format (yaml|json|table)")

	// Add namespace flags
	getCmd.PersistentFlags().StringVarP(&getNamespace, "namespace", "n", "", "Kubernetes namespace (default: current context namespace)")
	getCmd.PersistentFlags().BoolVarP(&getAllNamespaces, "all-namespaces", "A", false, "List resources across all namespaces")
}

func runGetTemplates(cmd *cobra.Command, args []string) error {
	templateNames, err := ListTemplates()
	if err != nil {
		return fmt.Errorf("failed to read templates: %v", err)
	}

	if len(templateNames) == 0 {
		fmt.Println("No templates found.")
		return nil
	}

	if outputFormat == "yaml" {
		var templates []ManifestInfo
		for _, templateName := range templateNames {
			manifestInfo, err := GetTemplateInfo(templateName)
			if err != nil {
				manifestInfo = ManifestInfo{
					Name:        templateName,
					Description: "No description available",
					FilePath:    fmt.Sprintf("%s.yaml", templateName),
				}
			}
			templates = append(templates, manifestInfo)
		}

		data, err := yaml.Marshal(templates)
		if err != nil {
			return fmt.Errorf("failed to marshal to YAML: %v", err)
		}
		fmt.Print(string(data))
		return nil
	}

	// Default table output
	var manifests []ManifestInfo
	for _, templateName := range templateNames {
		manifestInfo, err := GetTemplateInfo(templateName)
		if err != nil {
			manifestInfo = ManifestInfo{
				Name:        templateName,
				Description: "No description available",
				FilePath:    fmt.Sprintf("%s.yaml", templateName),
			}
		}
		manifests = append(manifests, manifestInfo)
	}

	// Print manifests in tabular format
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NAME\tDESCRIPTION")
	for _, manifest := range manifests {
		fmt.Fprintf(w, "%s\t%s\n", manifest.Name, manifest.Description)
	}

	return w.Flush()
}

func runGetTemplate(cmd *cobra.Command, args []string) error {
	templateName := args[0]

	if !TemplateExists(templateName) {
		return fmt.Errorf("template '%s' not found", templateName)
	}

	if outputFormat == "yaml" || outputFormat == "" {
		content, err := GetTemplateContent(templateName)
		if err != nil {
			return fmt.Errorf("failed to read template: %v", err)
		}
		fmt.Print(content)
		return nil
	}

	// For other output formats, show template info
	manifestInfo, err := GetTemplateInfo(templateName)
	if err != nil {
		return fmt.Errorf("failed to get template info: %v", err)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NAME\tDESCRIPTION")
	fmt.Fprintf(w, "%s\t%s\n", manifestInfo.Name, manifestInfo.Description)
	return w.Flush()
}

func getKthenaClient() (*versioned.Clientset, error) {
	config, err := clientcmd.BuildConfigFromFlags("", clientcmd.RecommendedHomeFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load kubeconfig: %v", err)
	}

	client, err := versioned.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create kthena client: %v", err)
	}

	return client, nil
}

func resolveGetNamespace() string {
	if getAllNamespaces {
		return ""
	}
	if getNamespace != "" {
		return getNamespace
	}
	return "default"
}

func runGetModelBoosters(cmd *cobra.Command, args []string) error {
	client, err := getKthenaClient()
	if err != nil {
		return err
	}

	namespace := resolveGetNamespace()
	ctx := context.Background()

	models, err := client.WorkloadV1alpha1().ModelBoosters(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list Models: %v", err)
	}

	// Get name filter if provided
	var nameFilter string
	if len(args) > 0 {
		nameFilter = args[0]
	}

	// Count matching models first
	matchCount := 0
	for _, model := range models.Items {
		if nameFilter == "" || strings.Contains(strings.ToLower(model.Name), strings.ToLower(nameFilter)) {
			matchCount++
		}
	}

	if matchCount == 0 {
		if nameFilter != "" {
			fmt.Printf("No Models found matching '%s'.\n", nameFilter)
		} else {
			if getAllNamespaces {
				fmt.Println("No Models found across all namespaces.")
			} else {
				fmt.Printf("No Models found in namespace %s.\n", namespace)
			}
		}
		return nil
	}

	// Print header
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	if getAllNamespaces {
		fmt.Fprintln(w, "NAMESPACE\tNAME\tAGE")
	} else {
		fmt.Fprintln(w, "NAME\tAGE")
	}

	// Print matching Models
	for _, model := range models.Items {
		if nameFilter == "" || strings.Contains(strings.ToLower(model.Name), strings.ToLower(nameFilter)) {
			age := time.Since(model.CreationTimestamp.Time).Truncate(time.Second)
			if getAllNamespaces {
				fmt.Fprintf(w, "%s\t%s\t%s\n", model.Namespace, model.Name, age)
			} else {
				fmt.Fprintf(w, "%s\t%s\n", model.Name, age)
			}
		}
	}

	return w.Flush()
}

func runGetModelServings(cmd *cobra.Command, args []string) error {
	client, err := getKthenaClient()
	if err != nil {
		return err
	}

	namespace := resolveGetNamespace()
	ctx := context.Background()

	modelServingList, err := client.WorkloadV1alpha1().ModelServings(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list ModelServings: %v", err)
	}

	if len(modelServingList.Items) == 0 {
		if getAllNamespaces {
			fmt.Println("No ModelServings found across all namespaces.")
		} else {
			fmt.Printf("No ModelServings found in namespace %s.\n", namespace)
		}
		return nil
	}

	// Print header
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	if getAllNamespaces {
		fmt.Fprintln(w, "NAMESPACE\tNAME\tAGE")
	} else {
		fmt.Fprintln(w, "NAME\tAGE")
	}

	// Print ModelServings
	for _, ms := range modelServingList.Items {
		age := time.Since(ms.CreationTimestamp.Time).Truncate(time.Second)
		if getAllNamespaces {
			fmt.Fprintf(w, "%s\t%s\t%s\n", ms.Namespace, ms.Name, age)
		} else {
			fmt.Fprintf(w, "%s\t%s\n", ms.Name, age)
		}
	}

	return w.Flush()
}

func runGetAutoscalingPolicies(cmd *cobra.Command, args []string) error {
	client, err := getKthenaClient()
	if err != nil {
		return err
	}

	namespace := resolveGetNamespace()
	ctx := context.Background()

	policies, err := client.WorkloadV1alpha1().AutoscalingPolicies(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list AutoscalingPolicies: %v", err)
	}

	if len(policies.Items) == 0 {
		if getAllNamespaces {
			fmt.Println("No AutoscalingPolicies found across all namespaces.")
		} else {
			fmt.Printf("No AutoscalingPolicies found in namespace %s.\n", namespace)
		}
		return nil
	}

	// Print header
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	if getAllNamespaces {
		fmt.Fprintln(w, "NAMESPACE\tNAME\tAGE")
	} else {
		fmt.Fprintln(w, "NAME\tAGE")
	}

	// Print AutoscalingPolicies
	for _, policy := range policies.Items {
		age := time.Since(policy.CreationTimestamp.Time).Truncate(time.Second)
		if getAllNamespaces {
			fmt.Fprintf(w, "%s\t%s\t%s\n", policy.Namespace, policy.Name, age)
		} else {
			fmt.Fprintf(w, "%s\t%s\n", policy.Name, age)
		}
	}

	return w.Flush()
}

func runGetAutoscalingPolicyBindings(cmd *cobra.Command, args []string) error {
	client, err := getKthenaClient()
	if err != nil {
		return err
	}

	namespace := resolveGetNamespace()
	ctx := context.Background()

	bindings, err := client.WorkloadV1alpha1().AutoscalingPolicyBindings(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list AutoscalingPolicyBindings: %v", err)
	}

	if len(bindings.Items) == 0 {
		if getAllNamespaces {
			fmt.Println("No AutoscalingPolicyBindings found across all namespaces.")
		} else {
			fmt.Printf("No AutoscalingPolicyBindings found in namespace %s.\n", namespace)
		}
		return nil
	}

	// Print header
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	if getAllNamespaces {
		fmt.Fprintln(w, "NAMESPACE\tNAME\tAGE")
	} else {
		fmt.Fprintln(w, "NAME\tAGE")
	}

	// Print AutoscalingPolicyBindings
	for _, binding := range bindings.Items {
		age := time.Since(binding.CreationTimestamp.Time).Truncate(time.Second)
		if getAllNamespaces {
			fmt.Fprintf(w, "%s\t%s\t%s\n", binding.Namespace, binding.Name, age)
		} else {
			fmt.Fprintf(w, "%s\t%s\n", binding.Name, age)
		}
	}

	return w.Flush()
}
