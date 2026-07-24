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
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/volcano-sh/kthena/client-go/clientset/versioned"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/engine"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"
)

var (
	templateName  string
	valuesFile    string
	dryRun        bool
	namespace     string
	name          string
	manifestFlags map[string]string
)

// createCmd represents the create command
var createCmd = &cobra.Command{
	Use:   "create",
	Short: "Create kthena resources",
	Long:  `Create kthena resources using predefined manifests and templates.`,
}

// manifestCmd represents the create manifest command
var manifestCmd = &cobra.Command{
	Use:   "manifest",
	Short: "Create resources from a manifest template",
	Long: `Create kthena resources from predefined manifest templates.

Manifests are predefined combinations of kthena resources that can be
customized with your specific values. This command will:
1. Render the template with your provided values
2. Show you the resulting YAML
3. Ask for confirmation
4. Apply the resources to Kubernetes (unless --dry-run is specified)

Examples:
  kthena create manifest --template basic-inference --name my-model --image my-registry/model:v1.0
  kthena create manifest --template basic-inference --values-file values.yaml
  kthena create manifest --template basic-inference --name my-model --dry-run`,
	RunE: runCreateManifest,
}

func init() {
	rootCmd.AddCommand(createCmd)
	createCmd.AddCommand(manifestCmd)

	// Manifest command flags
	manifestCmd.Flags().StringVarP(&templateName, "template", "t", "", "Template name to use (required)")
	manifestCmd.Flags().StringVarP(&valuesFile, "values-file", "f", "", "YAML file containing template values")
	manifestCmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show rendered template without applying to cluster")
	manifestCmd.Flags().StringVarP(&namespace, "namespace", "n", "default", "Kubernetes namespace to create resources in")
	manifestCmd.Flags().StringVar(&name, "name", "", "Name for the inference workload")

	// Dynamic flags for template values
	manifestFlags = make(map[string]string)
	manifestCmd.Flags().StringToStringVar(&manifestFlags, "set", nil, "Set template values (can specify multiple or separate values with commas: key1=val1,key2=val2)")

	if err := manifestCmd.MarkFlagRequired("template"); err != nil {
		panic(fmt.Sprintf("failed to mark template flag as required: %v", err))
	}
}

func runCreateManifest(cmd *cobra.Command, args []string) error {
	// Load template values
	values, err := loadTemplateValues()
	if err != nil {
		return fmt.Errorf("failed to load template values: %v", err)
	}

	// Render template
	renderedYAML, err := renderTemplate(templateName, values)
	if err != nil {
		return fmt.Errorf("failed to render template: %v", err)
	}

	// Show rendered YAML
	fmt.Println("Generated YAML:")
	fmt.Println("================")
	fmt.Println(renderedYAML)
	fmt.Println("================")

	if dryRun {
		fmt.Println("Dry run mode: not applying to cluster")
		return nil
	}

	// Ask for confirmation
	if !askForConfirmation("Do you want to apply these resources to the cluster?") {
		fmt.Println("Aborted.")
		return nil
	}

	// Apply resources to cluster
	return applyResources(renderedYAML)
}

func loadTemplateValues() (map[string]interface{}, error) {
	values := make(map[string]interface{})

	// Load from values file if specified
	if valuesFile != "" {
		data, err := os.ReadFile(valuesFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read values file: %v", err)
		}

		if err := yaml.Unmarshal(data, &values); err != nil {
			return nil, fmt.Errorf("failed to parse values file: %v", err)
		}
	}

	// Override with command line flags
	for key, value := range manifestFlags {
		values[key] = value
	}

	// Set name if specified via --name flag
	if name != "" {
		values["name"] = name
	}

	// Set default namespace if not specified
	if values["namespace"] == nil {
		values["namespace"] = namespace
	}

	return values, nil
}

func renderTemplate(templateName string, values map[string]interface{}) (string, error) {
	// Check if template exists in embedded files
	if !TemplateExists(templateName) {
		return "", fmt.Errorf("template '%s' not found", templateName)
	}

	// Get template content from embedded files
	templateData, err := GetTemplateContent(templateName)
	if err != nil {
		return "", fmt.Errorf("failed to read template: %v", err)
	}

	// Create Helm template engine
	helmEngine := engine.Engine{
		EnableDNS: false,
	}

	// Create a Helm chart structure
	helmChart := &chart.Chart{
		Metadata: &chart.Metadata{
			Name:    "kthena-template",
			Version: "1.0.0",
		},
		Templates: []*chart.File{
			{
				Name: "templates/" + templateName,
				Data: []byte(templateData),
			},
		},
	}

	// Wrap values under "Values" key for proper Helm template access
	helmValues := map[string]interface{}{
		"Values": values,
	}

	// Render template using Helm engine
	rendered, err := helmEngine.Render(helmChart, helmValues)
	if err != nil {
		return "", fmt.Errorf("failed to render template: %v", err)
	}

	// Get rendered content for our template
	templateKey := "kthena-template/templates/" + templateName
	renderedContent, exists := rendered[templateKey]
	if !exists {
		return "", fmt.Errorf("template '%s' was not rendered", templateName)
	}

	return renderedContent, nil
}

func askForConfirmation(question string) bool {
	fmt.Printf("%s [y/N]: ", question)
	reader := bufio.NewReader(os.Stdin)
	response, err := reader.ReadString('\n')
	if err != nil {
		return false
	}

	response = strings.ToLower(strings.TrimSpace(response))
	return response == "y" || response == "yes"
}

func applyResources(yamlContent string) error {
	// Load kubeconfig
	config, err := clientcmd.BuildConfigFromFlags("", clientcmd.RecommendedHomeFile)
	if err != nil {
		return fmt.Errorf("failed to load kubeconfig: %v", err)
	}

	// Create kthena client
	client, err := versioned.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create kthena client: %v", err)
	}

	ctx := context.Background()

	// Parse YAML documents
	decoder := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(yamlContent), 1024)

	for {
		var rawObj map[string]interface{}
		if err := decoder.Decode(&rawObj); err != nil {
			if err.Error() == "EOF" {
				break
			}
			return fmt.Errorf("failed to decode YAML: %v", err)
		}

		if rawObj == nil {
			continue
		}

		obj := &unstructured.Unstructured{Object: rawObj}

		// Get resource info
		gvk := obj.GetObjectKind().GroupVersionKind()
		resourceName := obj.GetName()
		resourceNamespace := obj.GetNamespace()

		fmt.Printf("Applying %s/%s: %s", gvk.Kind, gvk.Version, resourceName)
		if resourceNamespace != "" {
			fmt.Printf(" in namespace %s", resourceNamespace)
		}
		fmt.Println()

		// Apply based on resource type
		if err := applyKthenaResource(ctx, client, obj); err != nil {
			return fmt.Errorf("failed to apply %s %s: %v", gvk.Kind, resourceName, err)
		}

		fmt.Printf("  ✓ Applied successfully\n")
	}

	fmt.Println("All resources applied successfully!")
	return nil
}

func applyKthenaResource(ctx context.Context, client *versioned.Clientset, obj *unstructured.Unstructured) error {
	gvk := obj.GetObjectKind().GroupVersionKind()
	resourceName := obj.GetName()
	resourceNamespace := obj.GetNamespace()

	// Convert unstructured to the appropriate typed resource
	switch gvk.Kind {
	case "ModelServing":
		fmt.Printf("  Creating ModelServing: %s in namespace %s\n", resourceName, resourceNamespace)
		modelInfer := &workloadv1alpha1.ModelServing{}
		err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, modelInfer)
		if err != nil {
			return fmt.Errorf("failed to convert unstructured object to ModelServing: %v", err)
		}

		_, err = client.WorkloadV1alpha1().ModelServings(resourceNamespace).Create(ctx, modelInfer, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("failed to create ModelServing: %v", err)
		}

	case "ModelBooster":
		fmt.Printf("  Creating ModelBooster: %s in namespace %s\n", resourceName, resourceNamespace)
		model := &workloadv1alpha1.ModelBooster{}
		err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, model)
		if err != nil {
			return fmt.Errorf("failed to convert unstructured object to ModelBooster: %v", err)
		}

		_, err = client.WorkloadV1alpha1().ModelBoosters(resourceNamespace).Create(ctx, model, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("failed to create ModelBooster: %v", err)
		}

	case "AutoscalingPolicy":
		fmt.Printf("  Creating AutoscalingPolicy: %s in namespace %s\n", resourceName, resourceNamespace)
		policy := &workloadv1alpha1.AutoscalingPolicy{}
		err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, policy)
		if err != nil {
			return fmt.Errorf("failed to convert unstructured object to AutoscalingPolicy: %v", err)
		}

		_, err = client.WorkloadV1alpha1().AutoscalingPolicies(resourceNamespace).Create(ctx, policy, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("failed to create AutoscalingPolicy: %v", err)
		}

	default:
		return fmt.Errorf("unsupported resource type: %s", gvk.Kind)
	}

	return nil
}
