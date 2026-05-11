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
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "kthena",
	Short: "Kthena CLI for managing AI inference workloads",
	Long: `kthena is a CLI tool for managing Kthena AI inference workloads.

For detailed documentation, visit https://kthena.volcano.sh/

It allows you to:
- Create manifests from predefined templates with custom values
- List and view Kthena resources in Kubernetes clusters
- Manage inference workloads, models, and autoscaling policies

Examples:
  kthena get templates
  kthena describe template DeepSeek-R1-Distill-Qwen-32B
  kthena get template DeepSeek-R1-Distill-Qwen-32B -o yaml
  kthena create manifest --name my-model --template DeepSeek-R1-Distill-Qwen-32B
  kthena get model-boosters
  kthena get model-servings --all-namespaces`,
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

// GetRootCmd exports the root command for external tools (e.g., doc generation)
func GetRootCmd() *cobra.Command {
	return rootCmd
}

func init() {
	// Here you will define your flags and configuration settings.
	// Cobra supports persistent flags, which, if defined here,
	// will be global for your application.

	// rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default is $HOME/.kthena.yaml)")

	// Cobra also supports local flags, which will only run
	// when this action is called directly.
	rootCmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
}
