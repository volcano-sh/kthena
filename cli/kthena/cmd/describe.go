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
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// describeCmd represents the describe command
var describeCmd = &cobra.Command{
	Use:   "describe",
	Short: "Show detailed information about a specific resource",
	Long: `Show detailed information about a specific resource.

You can describe templates and other kthena resources.

Examples:
  kthena describe template DeepSeek-R1-Distill-Qwen-32B
  kthena describe model-booster my-model
  kthena describe model-serving my-serving
  kthena describe autoscaling-policy my-policy`,
}

// describeTemplateCmd represents the describe template command
var describeTemplateCmd = &cobra.Command{
	Use:   "template [NAME]",
	Short: "Show detailed information about a template",
	Long: `Show detailed information about a specific template.

This will display the template description, available variables,
and the full template content.`,
	Args: cobra.ExactArgs(1),
	RunE: runDescribeTemplate,
}

// describeModelBoosterCmd represents the describe model-booster command
var describeModelBoosterCmd = &cobra.Command{
	Use:   "model-booster [NAME]",
	Short: "Show detailed information about a model booster",
	Long: `Show detailed information about a specific ModelBooster resource in the cluster.

This will display the model booster configuration, status, and resource details.`,
	Args: cobra.ExactArgs(1),
	RunE: runDescribeModelBooster,
}

// describeModelServingCmd represents the describe model-serving command
var describeModelServingCmd = &cobra.Command{
	Use:     "model-serving [NAME]",
	Aliases: []string{"ms"},
	Short:   "Show detailed information about a model serving workload",
	Long: `Show detailed information about a specific ModelServing resource in the cluster.

This will display the model serving configuration, status, and resource details.`,
	Args: cobra.ExactArgs(1),
	RunE: runDescribeModelServing,
}

// describeAutoscalingPolicyCmd represents the describe autoscaling-policy command
var describeAutoscalingPolicyCmd = &cobra.Command{
	Use:     "autoscaling-policy [NAME]",
	Aliases: []string{"asp"},
	Short:   "Show detailed information about an autoscaling policy",
	Long: `Show detailed information about a specific AutoscalingPolicy resource in the cluster.

This will display the autoscaling policy configuration and rules.`,
	Args: cobra.ExactArgs(1),
	RunE: runDescribeAutoscalingPolicy,
}
var describeModelRouteCmd = &cobra.Command{
	Use:     "model-route [NAME]",
	Aliases: []string{"mroute"},
	Short:   "Show detailed information about a model route",
	Long: `Show detailed information about a specific ModelRoute resource in the cluster.

This will display the model route configuration including rules, target models, and rate limits.`,
	Args: cobra.ExactArgs(1),
	RunE: runDescribeModelRoute,
}

var describeModelServerCmd = &cobra.Command{
	Use:     "model-server [NAME]",
	Aliases: []string{"mserver"},
	Short:   "Show detailed information about a model server",
	Long: `Show detailed information about a specific ModelServer resource in the cluster.

This will display the model server configuration including inference engine, workload selector, and traffic policy.`,
	Args: cobra.ExactArgs(1),
	RunE: runDescribeModelServer,
}

func init() {
	rootCmd.AddCommand(describeCmd)
	describeCmd.AddCommand(describeTemplateCmd)
	describeCmd.AddCommand(describeModelBoosterCmd)
	describeCmd.AddCommand(describeModelServingCmd)
	describeCmd.AddCommand(describeAutoscalingPolicyCmd)
	describeCmd.AddCommand(describeModelRouteCmd)
	describeCmd.AddCommand(describeModelServerCmd)

	// Add namespace flags
	describeCmd.PersistentFlags().StringVarP(&getNamespace, "namespace", "n", "", "Kubernetes namespace (default: current context namespace)")
}

func runDescribeTemplate(cmd *cobra.Command, args []string) error {
	templateName := args[0]

	// Check if template exists
	if !TemplateExists(templateName) {
		return fmt.Errorf("template '%s' not found", templateName)
	}

	// Read template content from embedded files
	content, err := GetTemplateContent(templateName)
	if err != nil {
		return fmt.Errorf("failed to read template: %v", err)
	}
	fmt.Println("=================")
	fmt.Println("Template Content:")
	fmt.Println("=================")
	fmt.Println(content)

	return nil
}

func runDescribeModelBooster(cmd *cobra.Command, args []string) error {
	modelName := args[0]

	client, err := getKthenaClient()
	if err != nil {
		return err
	}

	namespace := getNamespace
	if namespace == "" {
		namespace = "default"
	}
	ctx := context.Background()

	model, err := client.WorkloadV1alpha1().ModelBoosters(namespace).Get(ctx, modelName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get Model '%s': %v", modelName, err)
	}

	fmt.Printf("Model: %s\n", model.Name)
	fmt.Println("================")
	fmt.Printf("Namespace: %s\n", model.Namespace)
	fmt.Printf("Created: %s\n", model.CreationTimestamp.Time.Format(time.RFC3339))
	fmt.Printf("Age: %s\n\n", time.Since(model.CreationTimestamp.Time).Truncate(time.Second))

	// Output the full resource as YAML
	data, err := yaml.Marshal(model)
	if err != nil {
		return fmt.Errorf("failed to marshal Model to YAML: %v", err)
	}

	fmt.Println("Resource Details:")
	fmt.Println("=================")
	fmt.Print(string(data))

	return nil
}

func runDescribeModelServing(cmd *cobra.Command, args []string) error {
	modelServingName := args[0]

	client, err := getKthenaClient()
	if err != nil {
		return err
	}

	namespace := getNamespace
	if namespace == "" {
		namespace = "default"
	}
	ctx := context.Background()

	modelServing, err := client.WorkloadV1alpha1().ModelServings(namespace).Get(ctx, modelServingName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get ModelServing '%s': %v", modelServingName, err)
	}

	fmt.Printf("ModelServing: %s\n", modelServing.Name)
	fmt.Println("================")
	fmt.Printf("Namespace: %s\n", modelServing.Namespace)
	fmt.Printf("Created: %s\n", modelServing.CreationTimestamp.Time.Format(time.RFC3339))
	fmt.Printf("Age: %s\n\n", time.Since(modelServing.CreationTimestamp.Time).Truncate(time.Second))

	// Output the full resource as YAML
	data, err := yaml.Marshal(modelServing)
	if err != nil {
		return fmt.Errorf("failed to marshal ModelServing to YAML: %v", err)
	}

	fmt.Println("Resource Details:")
	fmt.Println("=================")
	fmt.Print(string(data))

	return nil
}

func runDescribeAutoscalingPolicy(cmd *cobra.Command, args []string) error {
	policyName := args[0]

	client, err := getKthenaClient()
	if err != nil {
		return err
	}

	namespace := getNamespace
	if namespace == "" {
		namespace = "default"
	}
	ctx := context.Background()

	policy, err := client.WorkloadV1alpha1().AutoscalingPolicies(namespace).Get(ctx, policyName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get AutoscalingPolicy '%s': %v", policyName, err)
	}

	fmt.Printf("AutoscalingPolicy: %s\n", policy.Name)
	fmt.Println("================")
	fmt.Printf("Namespace: %s\n", policy.Namespace)
	fmt.Printf("Created: %s\n", policy.CreationTimestamp.Time.Format(time.RFC3339))
	fmt.Printf("Age: %s\n\n", time.Since(policy.CreationTimestamp.Time).Truncate(time.Second))

	// Output the full resource as YAML
	data, err := yaml.Marshal(policy)
	if err != nil {
		return fmt.Errorf("failed to marshal AutoscalingPolicy to YAML: %v", err)
	}

	fmt.Println("Resource Details:")
	fmt.Println("=================")
	fmt.Print(string(data))

	return nil
}
func runDescribeModelRoute(cmd *cobra.Command, args []string) error {
	routeName := args[0]

	client, err := getKthenaClient()
	if err != nil {
		return err
	}

	namespace := getNamespace
	if namespace == "" {
		namespace = "default"
	}
	ctx := context.Background()

	route, err := client.NetworkingV1alpha1().ModelRoutes(namespace).Get(ctx, routeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get ModelRoute '%s': %v", routeName, err)
	}

	fmt.Printf("ModelRoute: %s\n", route.Name)
	fmt.Println("================")
	fmt.Printf("Namespace: %s\n", route.Namespace)
	fmt.Printf("Created: %s\n", route.CreationTimestamp.Time.Format(time.RFC3339))
	fmt.Printf("Age: %s\n\n", time.Since(route.CreationTimestamp.Time).Truncate(time.Second))

	data, err := yaml.Marshal(route)
	if err != nil {
		return fmt.Errorf("failed to marshal ModelRoute to YAML: %v", err)
	}

	fmt.Println("Resource Details:")
	fmt.Println("=================")
	fmt.Print(string(data))

	return nil
}

func runDescribeModelServer(cmd *cobra.Command, args []string) error {
	serverName := args[0]

	client, err := getKthenaClient()
	if err != nil {
		return err
	}

	namespace := getNamespace
	if namespace == "" {
		namespace = "default"
	}
	ctx := context.Background()

	server, err := client.NetworkingV1alpha1().ModelServers(namespace).Get(ctx, serverName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get ModelServer '%s': %v", serverName, err)
	}

	fmt.Printf("ModelServer: %s\n", server.Name)
	fmt.Println("================")
	fmt.Printf("Namespace: %s\n", server.Namespace)
	fmt.Printf("Created: %s\n", server.CreationTimestamp.Time.Format(time.RFC3339))
	fmt.Printf("Age: %s\n\n", time.Since(server.CreationTimestamp.Time).Truncate(time.Second))

	data, err := yaml.Marshal(server)
	if err != nil {
		return fmt.Errorf("failed to marshal ModelServer to YAML: %v", err)
	}

	fmt.Println("Resource Details:")
	fmt.Println("=================")
	fmt.Print(string(data))

	return nil
}
